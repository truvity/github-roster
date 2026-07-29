package applier_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/truvity/github-roster/pkg/applier"
	"github.com/truvity/github-roster/pkg/peribolos"
)

const namespace = "roster"

func options() applier.Options {
	return applier.Options{
		Namespace:          namespace,
		Image:              "example.invalid/peribolos:v1",
		ServiceAccount:     "github-roster",
		Timeout:            time.Minute,
		MinAdmins:          1,
		MaxRemovalFraction: 0.4,
	}
}

// completeJobs makes the fake clientset behave like a cluster that runs
// Jobs: every created Job is immediately marked complete, so the runner's
// wait loop terminates.
func completeJobs(client *fake.Clientset, condition batchv1.JobConditionType) {
	client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		job, ok := action.(k8stesting.CreateAction).GetObject().(*batchv1.Job)
		if !ok {
			return false, nil, nil
		}

		job.Status.Conditions = []batchv1.JobCondition{{
			Type:    condition,
			Status:  corev1.ConditionTrue,
			Message: "from the test",
		}}

		return false, job, nil
	})
}

func result(mode peribolos.Mode) *peribolos.Result {
	return &peribolos.Result{
		Mode: mode,
		Org:  "example-org",
		YAML: "orgs:\n  example-org:\n    members:\n    - ada\n",
	}
}

func request(mode peribolos.Mode, confirm bool) applier.Request {
	return applier.Request{
		Result:            result(mode),
		Confirm:           confirm,
		CredentialsSecret: "roster-applier-example-org",
		AppID:             "4419264",
		InstallationID:    "149694485",
		RunID:             "test1",
		Actor:             "operator@example.com",
	}
}

func run(t *testing.T, req applier.Request) (*fake.Clientset, *applier.Run) {
	t.Helper()

	client := fake.NewSimpleClientset()
	completeJobs(client, batchv1.JobComplete)

	runner, err := applier.NewRunner(client, options())
	require.NoError(t, err)

	out, err := runner.Run(context.Background(), req)
	require.NoError(t, err)

	return client, out
}

func createdJob(t *testing.T, client *fake.Clientset, name string) *batchv1.Job {
	t.Helper()

	job, err := client.BatchV1().Jobs(namespace).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)

	return job
}

func args(t *testing.T, job *batchv1.Job) []string {
	t.Helper()

	require.Len(t, job.Spec.Template.Spec.Containers, 1)

	return job.Spec.Template.Spec.Containers[0].Args
}

func hasArg(list []string, prefix string) bool {
	for _, arg := range list {
		if arg == prefix || strings.HasPrefix(arg, prefix+"=") {
			return true
		}
	}

	return false
}

// A dry run must not carry --confirm. Without it peribolos reports what it
// would do and changes nothing, which is exactly the preview an operator
// sees before deciding.
func TestDryRunOmitsConfirm(t *testing.T) {
	t.Parallel()

	client, out := run(t, request(peribolos.ModeFull, false))

	require.False(t, out.Confirmed)
	require.True(t, out.Succeeded)
	require.Contains(t, out.JobName, "dryrun")

	require.False(t, hasArg(args(t, createdJob(t, client, out.JobName)), "--confirm"))
}

func TestConfirmedRunCarriesConfirm(t *testing.T) {
	t.Parallel()

	client, out := run(t, request(peribolos.ModeFull, true))

	require.True(t, out.Confirmed)
	require.True(t, hasArg(args(t, createdJob(t, client, out.JobName)), "--confirm"))
}

// What is ABSENT is the point: organization settings and team
// creation/deletion belong to the structure engine, and this service must
// never be the thing that changes them.
func TestJobNeverFixesOrgSettingsOrTeamExistence(t *testing.T) {
	t.Parallel()

	for _, mode := range []peribolos.Mode{peribolos.ModeRemovalsOnly, peribolos.ModeFull} {
		client, out := run(t, request(mode, true))
		list := args(t, createdJob(t, client, out.JobName))

		require.False(t, hasArg(list, "--fix-org"), "mode %s must not touch org settings", mode)
		require.False(t, hasArg(list, "--fix-teams"), "mode %s must not create or delete teams", mode)
		require.True(t, hasArg(list, "--fix-org-members"))
	}
}

// Team membership is an operator's decision; an unattended run touches
// organization membership only.
func TestOnlyFullSyncFixesTeamMembers(t *testing.T) {
	t.Parallel()

	client, out := run(t, request(peribolos.ModeRemovalsOnly, true))
	require.False(t, hasArg(args(t, createdJob(t, client, out.JobName)), "--fix-team-members"))

	client, out = run(t, request(peribolos.ModeFull, true))
	require.True(t, hasArg(args(t, createdJob(t, client, out.JobName)), "--fix-team-members"))
}

func TestGuardsArePassedToTheReconciler(t *testing.T) {
	t.Parallel()

	client, out := run(t, request(peribolos.ModeFull, true))
	list := args(t, createdJob(t, client, out.JobName))

	require.Contains(t, list, "--min-admins=1")
	require.Contains(t, list, "--maximum-removal-delta=0.4",
		"the configured shrink threshold must drive peribolos's own guard, so there is one number rather than two")
}

// The write credential is mounted into the Job and exists nowhere else in
// the request path.
func TestCredentialIsMountedNotPassed(t *testing.T) {
	t.Parallel()

	client, out := run(t, request(peribolos.ModeFull, true))
	job := createdJob(t, client, out.JobName)

	var secretVolume *corev1.Volume

	for i := range job.Spec.Template.Spec.Volumes {
		if job.Spec.Template.Spec.Volumes[i].Secret != nil {
			secretVolume = &job.Spec.Template.Spec.Volumes[i]
		}
	}

	require.NotNil(t, secretVolume, "the applier credential must arrive as a mounted Secret")
	require.Equal(t, "roster-applier-example-org", secretVolume.Secret.SecretName)

	for _, arg := range args(t, job) {
		require.NotContains(t, arg, "BEGIN", "a private key must never appear in an argument")
	}
}

// The rendered document is owned by the Job, so Kubernetes collects it with
// the Job and nothing carrying membership decisions is left in the
// namespace.
func TestConfigMapIsOwnedByTheJob(t *testing.T) {
	t.Parallel()

	client, out := run(t, request(peribolos.ModeFull, true))

	cm, err := client.CoreV1().ConfigMaps(namespace).Get(context.Background(), out.JobName, metav1.GetOptions{})
	require.NoError(t, err)

	require.Contains(t, cm.Data["config.yaml"], "example-org")
	require.Len(t, cm.OwnerReferences, 1)
	require.Equal(t, "Job", cm.OwnerReferences[0].Kind)
	require.Equal(t, out.JobName, cm.OwnerReferences[0].Name)
}

// A run that failed half-way must be looked at, not retried blindly
// against a live organization.
func TestJobDoesNotRetry(t *testing.T) {
	t.Parallel()

	client, out := run(t, request(peribolos.ModeFull, true))
	job := createdJob(t, client, out.JobName)

	require.NotNil(t, job.Spec.BackoffLimit)
	require.Zero(t, *job.Spec.BackoffLimit)
	require.Equal(t, corev1.RestartPolicyNever, job.Spec.Template.Spec.RestartPolicy)
}

func TestFailedJobIsReported(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()
	completeJobs(client, batchv1.JobFailed)

	runner, err := applier.NewRunner(client, options())
	require.NoError(t, err)

	out, err := runner.Run(context.Background(), request(peribolos.ModeFull, true))

	require.Error(t, err)
	require.False(t, out.Succeeded)
}

// The last line of defense before a real GitHub write: a document claiming
// to be removals-only must not contain additions, whatever produced it.
func TestRunnerRefusesARemovalsOnlyConfigThatAdds(t *testing.T) {
	t.Parallel()

	req := request(peribolos.ModeRemovalsOnly, true)
	req.Result.Adding = []string{"someone-new"}

	runner, err := applier.NewRunner(fake.NewSimpleClientset(), options())
	require.NoError(t, err)

	_, err = runner.Run(context.Background(), req)

	require.ErrorContains(t, err, "removals-only configuration would add")
}

func TestRunnerRefusesWithoutACredentialSecret(t *testing.T) {
	t.Parallel()

	req := request(peribolos.ModeFull, true)
	req.CredentialsSecret = ""

	runner, err := applier.NewRunner(fake.NewSimpleClientset(), options())
	require.NoError(t, err)

	_, err = runner.Run(context.Background(), req)

	require.ErrorContains(t, err, "mounted, never held by this process")
}

func TestRunnerRequiresItsOwnConfiguration(t *testing.T) {
	t.Parallel()

	_, err := applier.NewRunner(nil, options())
	require.ErrorContains(t, err, "kubernetes client")

	_, err = applier.NewRunner(fake.NewSimpleClientset(), applier.Options{Namespace: namespace})
	require.ErrorContains(t, err, "image is required")
}

// Labels are what makes a run findable afterwards — in kubectl, and in the
// audit record that points at it.
func TestJobIsLabelledForAudit(t *testing.T) {
	t.Parallel()

	client, out := run(t, request(peribolos.ModeRemovalsOnly, false))
	job := createdJob(t, client, out.JobName)

	require.Equal(t, "example-org", job.Labels["roster.truvity.io/org"])
	require.Equal(t, "removals-only", job.Labels["roster.truvity.io/mode"])
	require.Equal(t, "false", job.Labels["roster.truvity.io/confirmed"])
	require.Equal(t, "operator@example.com", job.Annotations["roster.truvity.io/actor"])
}
