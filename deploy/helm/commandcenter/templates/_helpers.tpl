{{- define "commandcenter.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "commandcenter.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "commandcenter.selectorLabels" -}}
app.kubernetes.io/name: {{ include "commandcenter.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "commandcenter.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "commandcenter.selectorLabels" . }}
app.kubernetes.io/version: {{ include "commandcenter.tag" . | trunc 63 | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "commandcenter.tag" -}}
{{- default .Chart.AppVersion .Values.image.tag }}
{{- end }}

{{/* repository:tag, plus @digest when image.digest pins it. */}}
{{- define "commandcenter.image" -}}
{{- .Values.image.repository }}:{{ include "commandcenter.tag" . }}{{ with .Values.image.digest }}@{{ . }}{{ end }}
{{- end }}

{{- define "commandcenter.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "commandcenter.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "commandcenter.configSecret" -}}
{{- default (printf "%s-config" (include "commandcenter.fullname" .)) .Values.config.existingSecret }}
{{- end }}

{{/* Settings that would otherwise be ignored without a word, or fail only after the install. */}}
{{- define "commandcenter.validate" -}}
{{- with .Values.config }}
{{- if and .existingSecret (or .baseURL .adminPasswordHash .anonymousRead .repositories .extra) }}
{{- fail "config.existingSecret is set, so config.baseURL, anonymousRead, adminPasswordHash, repositories and extra are not used: put them in that Secret's config.yaml" }}
{{- end }}
{{- if and $.Values.secretKey.existingSecret (hasKey (.extra | default dict) "secretKeyFile") }}
{{- fail "set secretKey.existingSecret or config.extra.secretKeyFile, not both: the server refuses to start with two keys" }}
{{- end }}
{{- end }}
{{- /* The pods' controller-revision-hash label is the StatefulSet's name plus 11 characters. */}}
{{- if gt (len (include "commandcenter.fullname" .)) 52 }}
{{- fail "the name is longer than 52 characters, too long for a StatefulSet: set fullnameOverride, or use a shorter release name" }}
{{- end }}
{{- end }}

{{/*
config.yaml rendered from .Values.config (not used with config.existingSecret). config.extra comes
first and the chart's keys go on top: listen and dataDir must match the pod's port and volume.
*/}}
{{- define "commandcenter.configYAML" -}}
{{- $c := deepCopy (.Values.config.extra | default dict) -}}
{{- $_ := set $c "listen" ":8081" -}}
{{- $_ := set $c "dataDir" "/data" -}}
{{- $_ := set $c "baseURL" (required "config.baseURL is required: the address clients use" .Values.config.baseURL) -}}
{{- $_ := set $c "anonymousRead" .Values.config.anonymousRead -}}
{{- if not (hasKey $c "users") }}
{{- $hash := required "config.adminPasswordHash is required: docker run --rm -i <image> hash-password" .Values.config.adminPasswordHash -}}
{{- $_ := set $c "users" (list (dict "name" "admin" "passwordHash" $hash "roles" (list "admin"))) -}}
{{- end }}
{{- with .Values.config.repositories }}{{ $_ := set $c "repositories" . }}{{ end -}}
{{- toYaml $c }}
{{- end }}

{{/*
Hash of the keys the server reads on every start. The others (config.AccessKeys and repositories
in the server) are imported into meta.db on the first start and ignored later, so a change to them
must not restart the pod for nothing.
*/}}
{{- define "commandcenter.configChecksum" -}}
{{- omit (include "commandcenter.configYAML" . | fromYaml) "users" "roles" "anonymousRead" "baseURL" "maxUploadMiB" "repositories" | toYaml | sha256sum }}
{{- end }}
