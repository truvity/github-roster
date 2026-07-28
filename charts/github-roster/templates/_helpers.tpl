{{- define "github-roster.fullname" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "github-roster.labels" -}}
app.kubernetes.io/name: github-roster
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}

{{- define "github-roster.selectorLabels" -}}
app.kubernetes.io/name: github-roster
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "github-roster.serviceAccountName" -}}
{{- .Values.serviceAccount.name | default (include "github-roster.fullname" .) }}
{{- end }}
