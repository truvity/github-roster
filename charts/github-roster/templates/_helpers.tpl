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

{{/*
The broker's selector labels use a DIFFERENT app name on purpose: broker
pods must never match the console Service's selector (which selects on
name+instance), or the console Service load-balances operator traffic
onto the broker — which happened on 2026-08-01, as intermittent 431s.
*/}}
{{- define "github-roster.brokerSelectorLabels" -}}
app.kubernetes.io/name: github-roster-broker
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
