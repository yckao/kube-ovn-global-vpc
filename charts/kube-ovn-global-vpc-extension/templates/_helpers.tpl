{{- define "native.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 50 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (.Values.nameOverride | default "native-extension") | trunc 50 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- define "native.targetIdentity" -}}
global-vpc-native-extension
{{- end -}}
{{- define "native.labels" -}}
app.kubernetes.io/name: kube-ovn-global-vpc-extension
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
{{- end -}}
{{- define "native.image" -}}
{{- if not (regexMatch "^sha256:[a-f0-9]{64}$" (.Values.cliImage.digest | default "")) -}}
{{- fail "cliImage.digest must be a released immutable sha256 digest; use a packaged release chart" -}}
{{- end -}}
{{- printf "%s@%s" (required "cliImage.repository is required" .Values.cliImage.repository) .Values.cliImage.digest -}}
{{- end -}}
