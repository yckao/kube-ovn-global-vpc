{{- define "global-vpc.fullname" -}}
{{- printf "%s-authority" .Release.Name | trunc 48 | trimSuffix "-" -}}
{{- end -}}
{{- define "global-vpc.guardRole" -}}
global-vpc-authority
{{- end -}}
{{- define "global-vpc.image" -}}
{{- $repo := required "image.repository must name a prebuilt release image" .repository -}}
{{- $digest := required "image.digest must contain the immutable sha256 digest" .digest -}}
{{- if not (regexMatch "^sha256:[a-f0-9]{64}$" $digest) -}}{{ fail "image digest must be sha256 plus 64 lowercase hexadecimal characters" }}{{- end -}}
{{- printf "%s@%s" $repo $digest -}}
{{- end -}}
{{- define "global-vpc.platform" -}}
{{- $config := deepCopy .Values.platform -}}
{{- if and $config.registryNamespace (ne $config.registryNamespace .Release.Namespace) -}}{{ fail "platform.registryNamespace must equal the release namespace" }}{{- end -}}
{{- $_ := set $config "registryNamespace" .Release.Namespace -}}
{{- if not $config.locations -}}{{ fail "platform.locations must register at least one location with explicit project grants and reserved CIDR pools" }}{{- end -}}
{{- $seen := dict -}}{{- $bindings := dict -}}
{{- range $loc := $config.locations -}}
{{- range $field := list "name" "region" "site" "dc" "bindingNamespace" -}}{{- if not (get $loc $field) -}}{{ fail (printf "each platform location requires %s" $field) }}{{- end -}}{{- end -}}
{{- if or (hasKey $seen $loc.name) (hasKey $bindings $loc.bindingNamespace) -}}{{ fail "location names and binding namespaces must be unique" }}{{- end -}}
{{- $_ := set $seen $loc.name true -}}{{- $_ := set $bindings $loc.bindingNamespace true -}}
{{- if or (not $loc.allowedProjects) (not $loc.cidrPools) -}}{{ fail "each location requires allowedProjects and cidrPools" }}{{- end -}}
{{- end -}}
{{- $config | toJson -}}
{{- end -}}
