{{- define "global-vpc-site.fullname" -}}
{{- printf "%s-site" .Release.Name | trunc 48 | trimSuffix "-" -}}
{{- end -}}
{{- define "global-vpc-site.guardRole" -}}
global-vpc-site
{{- end -}}
{{- define "global-vpc-site.image" -}}
{{- $repo := required "image.repository must name a prebuilt release image" .repository -}}
{{- $digest := required "image.digest must contain the immutable sha256 digest" .digest -}}
{{- if not (regexMatch "^sha256:[a-f0-9]{64}$" $digest) -}}{{ fail "image digest must be sha256 plus 64 lowercase hexadecimal characters" }}{{- end -}}
{{- printf "%s@%s" $repo $digest -}}
{{- end -}}
{{- define "global-vpc-site.secret" -}}
{{- if and .Values.authorityAccess.existingSecret .Values.authorityAccess.kubeconfig -}}{{ fail "set only one of authorityAccess.existingSecret and authorityAccess.kubeconfig" }}{{- end -}}
{{- if .Values.authorityAccess.existingSecret -}}{{ .Values.authorityAccess.existingSecret }}
{{- else if .Values.authorityAccess.kubeconfig -}}{{ include "global-vpc-site.fullname" . }}-authority-access
{{- else -}}{{ fail "authorityAccess.existingSecret or authorityAccess.kubeconfig is required" }}{{- end -}}
{{- end -}}
{{- define "global-vpc-site.local" -}}
{{- $config := deepCopy .Values.local -}}
{{- range $field := list "locationRef" "authorityNamespace" "transitPool" "bfdSourcePool" "controlPool" -}}
{{- if not (get $config $field) -}}{{ fail (printf "local.%s is required" $field) }}{{- end -}}
{{- end -}}
{{- if not $config.clusterUID -}}
{{- $identity := lookup "v1" "Namespace" "" "kube-system" -}}
{{- if or (not $identity) (not $identity.metadata.uid) -}}{{ fail "cannot resolve kube-system namespace UID; set local.clusterUID explicitly for offline Helm rendering" }}{{- end -}}
{{- $_ := set $config "clusterUID" $identity.metadata.uid -}}
{{- end -}}
{{- $_ := set $config "namespace" .Release.Namespace -}}
{{- $_ := set $config "authorityKubeconfig" "/authority/kubeconfig" -}}
{{- $_ := set $config "gatewayImage" (include "global-vpc-site.image" .Values.gatewayImage) -}}
{{- $config | toJson -}}
{{- end -}}
