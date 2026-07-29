// Package applier runs the reconciler as a short-lived Kubernetes Job.
//
// The Job boundary is the security design, not an implementation detail.
// The web tier holds a read-only GitHub credential; the write credential
// exists only as a Secret mounted into these Jobs. A compromise of the
// console can therefore read an organization and lie to its operators, but
// cannot mutate it — there is no code path from an HTTP handler to a GitHub
// write, only a path to "create a Job that has one".
//
// The reconciler itself is upstream peribolos, unmodified. It owns the
// parts of GitHub's membership model that are genuinely hard: invitation
// lifecycle, organization membership as distinct from team membership,
// pagination and rate limits.
package applier

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"

	"github.com/truvity/github-roster/pkg/peribolos"
)

// Defaults for the Job. Deliberately conservative: this is the component
// that changes people's access.
const (
	// configMountPath is where the rendered document is mounted.
	configMountPath = "/etc/peribolos"
	// credentialsMountPath is where the applier App's key is mounted.
	credentialsMountPath = "/etc/github" //nolint:gosec // G101: a mount path, not a credential
	// configFileName is the rendered document's filename.
	configFileName = "config.yaml"
	// privateKeyFileName is the App key's filename inside the Secret.
	privateKeyFileName = "github-private-key"

	// ttlAfterFinished lets Kubernetes garbage-collect a finished Job, and
	// with it the ConfigMap it owns.
	ttlAfterFinished int32 = 3600
	// backoffLimit is zero on purpose. A reconciler run that failed
	// half-way must be looked at, not retried blindly against a live
	// organization.
	backoffLimit int32 = 0

	pollInterval = 2 * time.Second
)

// Options configure the runner.
type Options struct {
	// Namespace is where Jobs and ConfigMaps are created. The service's
	// RBAC is scoped to this one namespace.
	Namespace string
	// Image is the upstream peribolos image.
	Image string
	// ServiceAccount the Job runs as.
	ServiceAccount string
	// Timeout bounds one run.
	Timeout time.Duration
	// MinAdmins is peribolos's own guard: it refuses a configuration with
	// fewer owners than this. Configurable because the upstream default of
	// five assumes an organization larger than ours, and a guard that is
	// always tripped is a guard nobody reads.
	MinAdmins int
	// MaxRemovalFraction refuses a run removing more than this share of an
	// organization. The circuit breaker against a directory returning
	// nonsense convincingly.
	MaxRemovalFraction float64
}

// Request is one reconciler run.
type Request struct {
	// Result is the rendered configuration to apply.
	Result *peribolos.Result
	// Confirm turns a dry run into a real one. False renders the preview
	// an operator sees before deciding.
	Confirm bool
	// CredentialsSecret holds the applier App's credentials. The web tier
	// never reads it; only the Job mounts it.
	CredentialsSecret string
	// AppID and InstallationID identify the applier App. They are not
	// secret, and passing them as arguments keeps the Secret to the one
	// thing that is.
	AppID          string
	InstallationID string
	// RunID namespaces the objects this run creates.
	RunID string
	// Actor is who asked for it, for the object labels and the record.
	Actor string
}

// Run is what happened.
type Run struct {
	JobName   string        `json:"jobName"`
	Confirmed bool          `json:"confirmed"`
	Succeeded bool          `json:"succeeded"`
	Output    string        `json:"output"`
	StartedAt time.Time     `json:"startedAt"`
	Duration  time.Duration `json:"duration"`
}

// Runner creates and supervises reconciler Jobs.
type Runner struct {
	client kubernetes.Interface
	opts   Options
}

// NewRunner returns a runner. It does not talk to the cluster.
func NewRunner(client kubernetes.Interface, opts Options) (*Runner, error) {
	switch {
	case client == nil:
		return nil, fmt.Errorf("a kubernetes client is required")
	case opts.Namespace == "":
		return nil, fmt.Errorf("namespace is required")
	case opts.Image == "":
		return nil, fmt.Errorf("reconciler image is required")
	}

	if opts.Timeout == 0 {
		opts.Timeout = 10 * time.Minute
	}

	if opts.MinAdmins == 0 {
		opts.MinAdmins = 1
	}

	return &Runner{client: client, opts: opts}, nil
}

// Run creates the ConfigMap and Job, waits for the Job, and returns its
// output.
//
// The ConfigMap is owned by the Job, so Kubernetes garbage-collects the
// rendered configuration along with it. Nothing carrying membership
// decisions is left lying around in the namespace.
func (r *Runner) Run(ctx context.Context, req Request) (*Run, error) {
	if req.Result == nil {
		return nil, fmt.Errorf("nothing to apply: no rendered configuration")
	}

	if err := r.validate(req); err != nil {
		return nil, err
	}

	name := objectName(req)
	started := time.Now()

	job, err := r.createJob(ctx, name, req)
	if err != nil {
		return nil, err
	}

	if err := r.createConfigMap(ctx, name, req, job); err != nil {
		return nil, err
	}

	run := &Run{JobName: name, Confirmed: req.Confirm, StartedAt: started}

	succeeded, err := r.wait(ctx, name)
	run.Succeeded = succeeded
	run.Duration = time.Since(started)
	run.Output = r.logs(ctx, name)

	if err != nil {
		return run, err
	}

	return run, nil
}

// validate refuses a run whose shape contradicts what it claims to be.
//
// The guard is here as well as in the renderer because this is the last
// place before a real GitHub write: a rendered document could reach a
// runner from anywhere, and "removals-only" must mean it.
func (r *Runner) validate(req Request) error {
	if req.Result.Mode == peribolos.ModeRemovalsOnly && len(req.Result.Adding) > 0 {
		return fmt.Errorf("refusing to run: a removals-only configuration would add %v", req.Result.Adding)
	}

	if req.CredentialsSecret == "" {
		return fmt.Errorf("no credentials secret: the write credential is mounted, never held by this process")
	}

	if req.AppID == "" || req.InstallationID == "" {
		return fmt.Errorf("applier app id and installation id are required")
	}

	return nil
}

// nameUnsafe matches everything a Kubernetes object name may not contain.
var nameUnsafe = regexp.MustCompile(`[^a-z0-9-]+`)

// maxNameLength is the API server's limit for these object names.
const maxNameLength = 63

// objectName builds a name the API server will accept.
//
// Sanitizing rather than trusting the input matters more than it looks: a
// fake client does NOT validate names, so an unsanitized run id containing
// an uppercase letter or a slash passes every test in this package and is
// rejected the first time somebody presses Sync. The failure would appear
// in production, on the one code path nobody wants to debug live.
func objectName(req Request) string {
	mode := "sync"
	if req.Result.Mode == peribolos.ModeRemovalsOnly {
		mode = "removals"
	}

	if !req.Confirm {
		mode += "-dryrun"
	}

	name := fmt.Sprintf("roster-%s-%s", mode, strings.ToLower(req.RunID))
	name = nameUnsafe.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")

	if len(name) > maxNameLength {
		name = strings.Trim(name[:maxNameLength], "-")
	}

	return name
}

// args builds the reconciler's command line.
//
// What is ABSENT matters as much as what is present:
//
//   - no --fix-org, so organization settings are never touched. They are
//     the structure engine's.
//   - no --fix-teams, so teams are never created or deleted. Also the
//     structure engine's. Only --fix-team-members, and only for a full
//     sync.
//   - --confirm only when an operator has confirmed. Without it peribolos
//     reports what it would do and changes nothing, which is exactly the
//     preview.
func (r *Runner) args(req Request) []string {
	args := []string{
		"--config-path=" + configMountPath + "/" + configFileName,
		"--github-app-id=" + req.AppID,
		"--github-app-private-key-path=" + credentialsMountPath + "/" + privateKeyFileName,
		"--fix-org-members",
		"--min-admins=" + strconv.Itoa(r.opts.MinAdmins),
		"--maximum-removal-delta=" + strconv.FormatFloat(r.removalDelta(), 'f', -1, 64),
	}

	if req.Result.Mode == peribolos.ModeFull {
		args = append(args, "--fix-team-members")
	}

	if req.Confirm {
		args = append(args, "--confirm")
	}

	return args
}

// removalDelta is peribolos's own shrink guard, driven by our configured
// threshold so there is one number rather than two that can disagree.
func (r *Runner) removalDelta() float64 {
	if r.opts.MaxRemovalFraction <= 0 || r.opts.MaxRemovalFraction > 1 {
		return 0.5
	}

	return r.opts.MaxRemovalFraction
}

func (r *Runner) labels(req Request) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "github-roster",
		"app.kubernetes.io/component": "reconciler",
		"roster.truvity.io/org":       req.Result.Org,
		"roster.truvity.io/mode":      string(req.Result.Mode),
		"roster.truvity.io/run":       req.RunID,
		"roster.truvity.io/confirmed": strconv.FormatBool(req.Confirm),
	}
}

func (r *Runner) createJob(ctx context.Context, name string, req Request) (*batchv1.Job, error) {
	deadline := int64(r.opts.Timeout.Seconds())
	ttl := ttlAfterFinished
	backoff := backoffLimit

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   r.opts.Namespace,
			Labels:      r.labels(req),
			Annotations: map[string]string{"roster.truvity.io/actor": req.Actor},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: r.labels(req)},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: r.opts.ServiceAccount,
					Containers: []corev1.Container{{
						Name:  "peribolos",
						Image: r.opts.Image,
						Args:  r.args(req),
						VolumeMounts: []corev1.VolumeMount{
							{Name: "config", MountPath: configMountPath, ReadOnly: true},
							{Name: "credentials", MountPath: credentialsMountPath, ReadOnly: true},
						},
					}},
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: name},
								},
							},
						},
						{
							// The write credential, reachable only from
							// inside this Job.
							Name: "credentials",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{SecretName: req.CredentialsSecret},
							},
						},
					},
				},
			},
		},
	}

	created, err := r.client.BatchV1().Jobs(r.opts.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create job %q: %w", name, err)
	}

	return created, nil
}

// createConfigMap writes the rendered document, owned by the Job so it is
// garbage-collected with it.
func (r *Runner) createConfigMap(ctx context.Context, name string, req Request, job *batchv1.Job) error {
	owner := metav1.OwnerReference{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Name:       job.Name,
		UID:        job.UID,
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       r.opts.Namespace,
			Labels:          r.labels(req),
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Data: map[string]string{configFileName: req.Result.YAML},
	}

	if _, err := r.client.CoreV1().ConfigMaps(r.opts.Namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create configmap %q: %w", name, err)
	}

	return nil
}

// wait blocks until the Job finishes.
func (r *Runner) wait(ctx context.Context, name string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()

	var succeeded bool

	err := wait.PollUntilContextCancel(ctx, pollInterval, true, func(ctx context.Context) (bool, error) {
		job, err := r.client.BatchV1().Jobs(r.opts.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}

			return false, err
		}

		for _, condition := range job.Status.Conditions {
			if condition.Status != corev1.ConditionTrue {
				continue
			}

			switch condition.Type {
			case batchv1.JobComplete:
				succeeded = true

				return true, nil
			case batchv1.JobFailed:
				return true, fmt.Errorf("reconciler job %q failed: %s", name, condition.Message)
			}
		}

		return false, nil
	})
	if err != nil {
		return succeeded, err
	}

	return succeeded, nil
}

// logs returns the Job's pod output, which is the diff peribolos computed.
//
// Best-effort: the output is valuable but never the reason a run is
// considered failed, and the audit record says so when it is missing.
func (r *Runner) logs(ctx context.Context, jobName string) string {
	pods, err := r.client.CoreV1().Pods(r.opts.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	if err != nil || len(pods.Items) == 0 {
		return ""
	}

	var out strings.Builder

	for i := range pods.Items {
		stream, err := r.client.CoreV1().Pods(r.opts.Namespace).
			GetLogs(pods.Items[i].Name, &corev1.PodLogOptions{}).Stream(ctx)
		if err != nil {
			continue
		}

		buf := make([]byte, 64<<10)

		n, _ := stream.Read(buf)
		_ = stream.Close()

		out.Write(buf[:n])
	}

	return out.String()
}
