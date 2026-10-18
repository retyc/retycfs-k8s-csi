{{/*
Expand the name of the chart.
*/}}
{{- define "retycfs-csi.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name, truncated to the DNS label limit.
*/}}
{{- define "retycfs-csi.fullname" -}}
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

{{- define "retycfs-csi.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
The CSI driver name is compiled into the binary and cannot be changed here.
*/}}
{{- define "retycfs-csi.driverName" -}}
csi.retycfs.com
{{- end }}

{{- define "retycfs-csi.labels" -}}
helm.sh/chart: {{ include "retycfs-csi.chart" . }}
{{ include "retycfs-csi.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "retycfs-csi.selectorLabels" -}}
app.kubernetes.io/name: {{ include "retycfs-csi.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "retycfs-csi.image" -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) }}
{{- end }}

{{- define "retycfs-csi.controller.serviceAccountName" -}}
{{- if .Values.serviceAccount.controller.create }}
{{- default (printf "%s-controller" (include "retycfs-csi.fullname" .)) .Values.serviceAccount.controller.name }}
{{- else }}
{{- required "serviceAccount.controller.name is required when serviceAccount.controller.create is false (the default ServiceAccount must not be used)" .Values.serviceAccount.controller.name }}
{{- end }}
{{- end }}

{{- define "retycfs-csi.node.serviceAccountName" -}}
{{- if .Values.serviceAccount.node.create }}
{{- default (printf "%s-node" (include "retycfs-csi.fullname" .)) .Values.serviceAccount.node.name }}
{{- else }}
{{- required "serviceAccount.node.name is required when serviceAccount.node.create is false (the default ServiceAccount must not be used)" .Values.serviceAccount.node.name }}
{{- end }}
{{- end }}

{{- define "retycfs-csi.enroller.serviceAccountName" -}}
{{- if .Values.serviceAccount.enroller.create }}
{{- default (printf "%s-enroller" (include "retycfs-csi.fullname" .)) .Values.serviceAccount.enroller.name }}
{{- else }}
{{- required "serviceAccount.enroller.name is required when serviceAccount.enroller.create is false (the default ServiceAccount must not be used)" .Values.serviceAccount.enroller.name }}
{{- end }}
{{- end }}

{{/*
Secret of the cluster-wide identity, written by the enroller (internal/enroller/secret.go); a
tenant's is this name, "-" and its namespace.
*/}}
{{- define "retycfs-csi.identitySecretName" -}}
retycfs-identity
{{- end }}

{{/*
Type of the identities' Secrets, the only ones the enroller writes.
*/}}
{{- define "retycfs-csi.identitySecretType" -}}
csi.retycfs.com/machine-identity
{{- end }}

{{/*
Where the driver reads the cluster-wide identity (--identity-dir, its default).
*/}}
{{- define "retycfs-csi.identityDir" -}}
/var/run/retycfs-csi/identity
{{- end }}

{{/*
Fails on the values of the token identity, removed: a forgotten one must not leave a release
without the identity it expects.
*/}}
{{- define "retycfs-csi.validate" -}}
{{- if hasKey .Values "credentials" }}
{{- fail "credentials.* is gone (RETYC_TOKEN, RETYC_KEY_PASSPHRASE): the driver enrolls a Retyc machine account instead, set identity.organizationID (see NOTES)" }}
{{- end }}
{{- range .Values.storageClasses }}
{{- if hasKey . "tenantSecretName" }}
{{- fail (printf "storageClasses[%s].tenantSecretName is gone: set tenant: true (the Secret is retycfs-identity-<namespace>, made by the enroller)" .name) }}
{{- end }}
{{- end }}
{{- end }}

{{/*
True when at least one StorageClass uses per-namespace identities.
*/}}
{{- define "retycfs-csi.usesTenantSecrets" -}}
{{- $uses := false }}
{{- range .Values.storageClasses }}{{ if .tenant }}{{ $uses = true }}{{ end }}{{ end }}
{{- $uses }}
{{- end }}

{{/*
The StorageClasses this release renders, as a JSON list of names: the tenant ones, and the others
only with a cluster-wide identity.
*/}}
{{- define "retycfs-csi.storageClassNames" -}}
{{- $names := list }}
{{- range .Values.storageClasses }}
{{- if or .tenant $.Values.identity.organizationID }}{{ $names = append $names .name }}{{ end }}
{{- end }}
{{- $names | toJson }}
{{- end }}

{{/*
Volume and mount of the cluster-wide identity: optional, it exists once enrolled; the kubelet
refreshes it in place when approved. The driver runs as root and reads it at each use.
*/}}
{{- define "retycfs-csi.identityVolume" -}}
- name: identity
  secret:
    secretName: {{ include "retycfs-csi.identitySecretName" . }}
    optional: true
    defaultMode: 0400
{{- end }}

{{- define "retycfs-csi.identityVolumeMount" -}}
- name: identity
  mountPath: {{ include "retycfs-csi.identityDir" . }}
  readOnly: true
{{- end }}

{{/*
Arguments of the driver on the cluster-wide identity. The pods are Ready without it: its approval
shows on the RetycIdentity, not on the driver, so that `helm install --wait` does not wait for it.
*/}}
{{- define "retycfs-csi.identityArgs" -}}
- --identity-dir={{ include "retycfs-csi.identityDir" . }}
{{- end }}

{{/*
"true" when csi-provisioner and csi-resizer run with --leader-election: more than one controller
replica, or the flag set by hand in either sidecar's extraArgs.
*/}}
{{- define "retycfs-csi.leaderElection" -}}
{{- $c := .Values.controller }}
{{- or (gt (int $c.replicas) 1) (has "--leader-election" $c.provisioner.extraArgs) (has "--leader-election" $c.resizer.extraArgs) }}
{{- end }}

{{/*
Secret and key holding RETYC_CACHE_REDIS_URL for the node plugin, as "name/key", or "" with the
memory cache. Fails on cache.store: redis without a Redis, and on the subchart with another store:
it would keep running unused, its password Secret gone (redis-secret.yaml), and a later return to
redis would generate a new password the running Redis does not know.
*/}}
{{- define "retycfs-csi.redisURLSecret" -}}
{{- $c := .Values.cache }}
{{- if eq $c.store "redis" }}
{{- if and $c.redis.subchart.enabled $c.redis.subchart.existingSecret }}
{{- $auth := dig "auth" "existingSecret" "" (.Values.redis | default dict) }}
{{- if ne $auth $c.redis.subchart.existingSecret }}
{{- fail (printf "cache.redis.subchart.existingSecret is %q: set redis.auth.existingSecret to the same Secret (now %q), for the Redis subchart to read its password there" $c.redis.subchart.existingSecret $auth) }}
{{- end }}
{{- printf "%s/RETYC_CACHE_REDIS_URL" $c.redis.subchart.existingSecret }}
{{- else if $c.redis.subchart.enabled }}
{{- printf "%s/RETYC_CACHE_REDIS_URL" (include "retycfs-csi.redisAuthSecretName" .) }}
{{- else if $c.redis.external.existingSecret }}
{{- printf "%s/%s" $c.redis.external.existingSecret $c.redis.external.existingSecretKey }}
{{- else if $c.redis.external.url }}
{{- printf "%s/RETYC_CACHE_REDIS_URL" (include "retycfs-csi.redisAuthSecretName" .) }}
{{- else }}
{{- fail "cache.store is redis: enable cache.redis.subchart or set cache.redis.external.url or existingSecret" }}
{{- end }}
{{- else if $c.redis.subchart.enabled }}
{{- fail (printf "cache.redis.subchart.enabled needs cache.store: redis (now %q): disable the subchart with the memory cache" $c.store) }}
{{- end }}
{{- end }}

{{/*
Name of the Secret the chart creates for Redis (subchart password and URL, or the external URL).
Built from the release name alone: the subchart's auth.existingSecret refers to it the same way.
*/}}
{{- define "retycfs-csi.redisAuthSecretName" -}}
{{- printf "%s-redis-auth" .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Service of the Redis subchart, as the CloudPirates chart names it (cloudpirates.fullname).
*/}}
{{- define "retycfs-csi.redisSubchartHost" -}}
{{- $r := .Values.redis | default dict }}
{{- $name := "" }}
{{- if $r.fullnameOverride }}
{{- $name = $r.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $base := $r.nameOverride | default "redis" }}
{{- if contains $base .Release.Name }}
{{- $name = .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name = printf "%s-%s" .Release.Name $base | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- printf "%s.%s.svc" $name (($r.namespaceOverride | default .Release.Namespace)) }}
{{- end }}
