{{/* Names and labels ------------------------------------------------------ */}}

{{- define "taut.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "taut.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 52 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 52 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 52 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "taut.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Labels shared by every pod of the release; the default-deny policy selects on these. */}}
{{- define "taut.releaseSelector" -}}
app.kubernetes.io/name: {{ include "taut.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* taut.selectorLabels (dict "root" . "component" "worker") */}}
{{- define "taut.selectorLabels" -}}
{{ include "taut.releaseSelector" .root }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{/* taut.labels (dict "root" . "component" "worker"); component is optional. */}}
{{- define "taut.labels" -}}
helm.sh/chart: {{ include "taut.chart" .root }}
{{ include "taut.releaseSelector" .root }}
{{- if .component }}
app.kubernetes.io/component: {{ .component }}
{{- end }}
app.kubernetes.io/version: {{ .root.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
app.kubernetes.io/part-of: taut
{{- end -}}

{{- define "taut.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
{{- end -}}

{{- define "taut.imagePullSecrets" -}}
{{- with .Values.imagePullSecrets }}
imagePullSecrets:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end -}}

{{/* taut.serviceAccountName (dict "root" . "component" "worker") */}}
{{- define "taut.serviceAccountName" -}}
{{- $sa := (index .root.Values .component).serviceAccount -}}
{{- if $sa.create -}}
{{- default (printf "%s-%s" (include "taut.fullname" .root) .component) $sa.name -}}
{{- else -}}
{{- default "default" $sa.name -}}
{{- end -}}
{{- end -}}

{{/* Modes ----------------------------------------------------------------- */}}

{{/* "true" when the worker pod has its token broker and egress proxy. */}}
{{- define "taut.sidecars" -}}
{{- if not (and .Values.dev.insecure .Values.dev.noSidecars) -}}true{{- end -}}
{{- end -}}

{{/* "true" when a Slack token is configured. */}}
{{- define "taut.hasSlackToken" -}}
{{- if or .Values.slack.token.existingSecret (and .Values.secrets.create .Values.secrets.slackToken) -}}true{{- end -}}
{{- end -}}

{{/* Secrets --------------------------------------------------------------- */}}

{{- define "taut.devSecretName" -}}
{{- printf "%s-dev" (include "taut.fullname" .) -}}
{{- end -}}

{{/*
taut.secretName (dict "root" . "existing" .Values.postgres.existingSecret "value" "postgres.existingSecret" "dev" "postgresUrl" "what" "the Postgres connection URL")
The existing Secret's name, or the dev Secret when secrets.create supplies the value.
*/}}
{{- define "taut.secretName" -}}
{{- if .existing -}}
{{- .existing -}}
{{- else if and .root.Values.secrets.create (index .root.Values.secrets .dev) -}}
{{- include "taut.devSecretName" .root -}}
{{- else if .root.Values.secrets.create -}}
{{- fail (printf "\n\n%s is not set and secrets.%s is empty.\nSet %s to the name of an existing Secret holding %s, or (development only) set secrets.%s." .value .dev .value .what .dev) -}}
{{- else -}}
{{- fail (printf "\n\n%s is required: the name of an existing Secret holding %s.\nCreate it with kubectl (docs/OPERATIONS.md, \"Secrets\") and set %s." .value .what .value) -}}
{{- end -}}
{{- end -}}

{{- define "taut.postgresSecret" -}}
{{- include "taut.secretName" (dict "root" . "existing" .Values.postgres.existingSecret "value" "postgres.existingSecret" "dev" "postgresUrl" "what" "the Postgres connection URL") -}}
{{- end -}}
{{- define "taut.stagingKeySecret" -}}
{{- include "taut.secretName" (dict "root" . "existing" .Values.stagingKey.existingSecret "value" "stagingKey.existingSecret" "dev" "stagingKey" "what" "the staging key (`taut dev keygen`)") -}}
{{- end -}}
{{- define "taut.controllerTokenSecret" -}}
{{- include "taut.secretName" (dict "root" . "existing" .Values.controllerToken.existingSecret "value" "controllerToken.existingSecret" "dev" "controllerToken" "what" "the controller's API token") -}}
{{- end -}}
{{- define "taut.s3Secret" -}}
{{- include "taut.secretName" (dict "root" . "existing" .Values.s3.existingSecret "value" "s3.existingSecret" "dev" "s3AccessKey" "what" "the S3 access key and secret key") -}}
{{- end -}}
{{- define "taut.entraSecret" -}}
{{- $dev := ternary "entraCertificate" "entraClientSecret" (eq .Values.entra.credential.type "certificate") -}}
{{- include "taut.secretName" (dict "root" . "existing" .Values.entra.credential.existingSecret "value" "entra.credential.existingSecret" "dev" $dev "what" (printf "the Entra application's %s" (ternary "certificate and private key (PEM)" "client secret" (eq .Values.entra.credential.type "certificate")))) -}}
{{- end -}}
{{- define "taut.slackSecret" -}}
{{- default (include "taut.devSecretName" .) .Values.slack.token.existingSecret -}}
{{- end -}}

{{/* Key of the Entra credential in its Secret. */}}
{{- define "taut.entraKey" -}}
{{- default (ternary "certificate.pem" "client-secret" (eq .Values.entra.credential.type "certificate")) .Values.entra.credential.key -}}
{{- end -}}

{{/* File name of the Entra credential under /run/secrets/taut. */}}
{{- define "taut.entraFile" -}}
{{- ternary "entra-certificate.pem" "entra-client-secret" (eq .Values.entra.credential.type "certificate") -}}
{{- end -}}

{{/*
The volume holding the Microsoft and Slack credentials. Mounted by the token
broker only (or, with dev.noSidecars, by the worker).
*/}}
{{- define "taut.credentialsVolume" -}}
- name: credentials
  projected:
    defaultMode: 0440
    sources:
      - secret:
          name: {{ include "taut.entraSecret" . }}
          items:
            - key: {{ include "taut.entraKey" . }}
              path: {{ include "taut.entraFile" . }}
      {{- if include "taut.hasSlackToken" . }}
      - secret:
          name: {{ include "taut.slackSecret" . }}
          items:
            - key: {{ .Values.slack.token.key }}
              path: slack-token
      {{- end }}
{{- end -}}

{{/* Credentials as environment variables, for `env:` references in config files. */}}
{{- define "taut.credentialsEnv" -}}
- name: {{ ternary "TAUT_ENTRA_CERTIFICATE" "TAUT_ENTRA_CLIENT_SECRET" (eq .Values.entra.credential.type "certificate") }}
  valueFrom:
    secretKeyRef:
      name: {{ include "taut.entraSecret" . }}
      key: {{ include "taut.entraKey" . }}
{{- if include "taut.hasSlackToken" . }}
- name: TAUT_SLACK_TOKEN
  valueFrom:
    secretKeyRef:
      name: {{ include "taut.slackSecret" . }}
      key: {{ .Values.slack.token.key }}
{{- end }}
{{- end -}}

{{/*
taut.backingVolume (dict "root" . "token" true)
What the controller and the workers both need to reach Postgres and staging,
as files. The controller's also carries its API token.
*/}}
{{- define "taut.backingVolume" -}}
{{- $v := .root.Values -}}
- name: backing
  projected:
    defaultMode: 0440
    sources:
      - secret:
          name: {{ include "taut.postgresSecret" .root }}
          items:
            - key: {{ $v.postgres.key }}
              path: postgres-url
      - secret:
          name: {{ include "taut.stagingKeySecret" .root }}
          items:
            - key: {{ $v.stagingKey.key }}
              path: staging-key
      - secret:
          name: {{ include "taut.s3Secret" .root }}
          items:
            - key: {{ $v.s3.accessKeyKey }}
              path: s3-access-key
            - key: {{ $v.s3.secretKeyKey }}
              path: s3-secret-key
      {{- if and .token (not (and $v.dev.insecure $v.dev.controllerInsecure)) }}
      - secret:
          name: {{ include "taut.controllerTokenSecret" .root }}
          items:
            - key: {{ $v.controllerToken.key }}
              path: controller-token
      {{- end }}
{{- end -}}

{{/* Environment ------------------------------------------------------------ */}}

{{/* Postgres, staging key and S3 for the controller and the workers. */}}
{{- define "taut.backingEnv" -}}
- name: TAUT_POSTGRES_URL_FILE
  value: /run/secrets/taut-backing/postgres-url
- name: TAUT_STAGING_KEY_FILE
  value: /run/secrets/taut-backing/staging-key
- name: TAUT_STAGING_BACKEND
  value: s3
{{- with .Values.s3.endpoint }}
- name: TAUT_S3_ENDPOINT
  value: {{ . | quote }}
{{- end }}
- name: TAUT_S3_REGION
  value: {{ .Values.s3.region | quote }}
- name: TAUT_S3_BUCKET
  value: {{ .Values.s3.bucket | quote }}
- name: TAUT_S3_ACCESS_KEY_FILE
  value: /run/secrets/taut-backing/s3-access-key
- name: TAUT_S3_SECRET_KEY_FILE
  value: /run/secrets/taut-backing/s3-secret-key
{{- end -}}

{{/* "true" when a container needs the NATS files volume. */}}
{{- define "taut.natsFiles" -}}
{{- if or .Values.nats.credentials.existingSecret .Values.nats.tls.existingSecret -}}true{{- end -}}
{{- end -}}

{{/*
taut.natsEnv .
The URL is a plain value; whatever identifies the process to NATS is a file.
*/}}
{{- define "taut.natsEnv" -}}
{{- $n := .Values.nats -}}
- name: TAUT_NATS_URL
  value: {{ $n.url | quote }}
{{- if $n.credentials.existingSecret }}
- name: TAUT_NATS_CREDS_FILE
  value: /run/secrets/taut-nats/nats.creds
{{- end }}
{{- if and $n.tls.existingSecret $n.tls.caKey }}
- name: TAUT_NATS_CA_FILE
  value: /run/secrets/taut-nats/ca.crt
{{- end }}
{{- if and $n.tls.existingSecret $n.tls.certKey }}
- name: TAUT_NATS_CERT_FILE
  value: /run/secrets/taut-nats/tls.crt
- name: TAUT_NATS_KEY_FILE
  value: /run/secrets/taut-nats/tls.key
{{- end }}
{{- end -}}

{{/*
taut.natsVolume (dict "root" . "name" "nats-worker" "key" .Values.nats.credentials.workerKey)
One volume per container, holding that identity's credentials file only.
*/}}
{{- define "taut.natsVolume" -}}
{{- $n := .root.Values.nats -}}
{{- if include "taut.natsFiles" .root }}
- name: {{ .name }}
  projected:
    defaultMode: 0440
    sources:
      {{- if $n.credentials.existingSecret }}
      - secret:
          name: {{ $n.credentials.existingSecret }}
          items:
            - key: {{ .key }}
              path: nats.creds
      {{- end }}
      {{- if $n.tls.existingSecret }}
      - secret:
          name: {{ $n.tls.existingSecret }}
          items:
            {{- with $n.tls.caKey }}
            - key: {{ . }}
              path: ca.crt
            {{- end }}
            {{- with $n.tls.certKey }}
            - key: {{ . }}
              path: tls.crt
            - key: {{ $n.tls.keyKey }}
              path: tls.key
            {{- end }}
      {{- end }}
{{- end }}
{{- end -}}

{{/* taut.natsMount (dict "root" . "name" "nats-worker") */}}
{{- define "taut.natsMount" -}}
{{- if include "taut.natsFiles" .root }}
- name: {{ .name }}
  mountPath: /run/secrets/taut-nats
  readOnly: true
{{- end }}
{{- end -}}

{{- define "taut.budgetEnv" -}}
- name: TAUT_BUDGET_GRAPH_RPS
  value: {{ .Values.budgets.graphRps | quote }}
- name: TAUT_BUDGET_SHAREPOINT_RPS
  value: {{ .Values.budgets.sharepointRps | quote }}
- name: TAUT_BUDGET_SLACK_RPS
  value: {{ .Values.budgets.slackRps | quote }}
{{- end -}}

{{- define "taut.telemetryEnv" -}}
{{- if .Values.telemetry.endpoint }}
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  value: {{ .Values.telemetry.endpoint | quote }}
{{- with .Values.telemetry.metricExportIntervalMs }}
- name: OTEL_METRIC_EXPORT_INTERVAL
  value: {{ . | quote }}
{{- end }}
{{- with .Values.telemetry.env }}
{{ toYaml . }}
{{- end }}
{{- end }}
{{- end -}}

{{- define "taut.proxyAddr" -}}
{{- printf "127.0.0.1:%d" (int .Values.egressProxy.port) -}}
{{- end -}}

{{/* Validation ------------------------------------------------------------- */}}

{{/*
Everything that must stop a render. Included once, from validate.yaml.
*/}}
{{- define "taut.validate" -}}
{{- $v := .Values -}}

{{/* 1. The insecure toggles need dev.insecure. */}}
{{- if not $v.dev.insecure -}}
  {{- range $name := list "controllerInsecure" "allowTestEndpoints" "noSidecars" "tokenAllowEndpoints" "entraAuthority" "graphUrl" "slackFilesUrl" -}}
    {{- if index $v.dev $name -}}
      {{- fail (printf "\n\ndev.%s is set but dev.insecure is not true.\nThe dev.* toggles remove protections a real migration depends on (an unauthenticated API, credentials sent to endpoints of your choosing, credentials inside the worker). If this is a throwaway stack with fake Microsoft and Slack services, set dev.insecure=true as well; otherwise remove dev.%s." $name $name) -}}
    {{- end -}}
  {{- end -}}
  {{- range $entry := $v.egressProxy.extraAllow -}}
    {{- if contains "http://" $entry -}}
      {{- fail (printf "\n\negressProxy.extraAllow entry %q is plain http, which would send tokens unencrypted.\nUse an https destination, or set dev.insecure=true for a throwaway stack." $entry) -}}
    {{- end -}}
  {{- end -}}
{{- end -}}

{{/* 2. The same switches may not be slipped in through extraEnv. */}}
{{- $forbidden := list "TAUT_CONTROLLER_INSECURE" "TAUT_ALLOW_TEST_ENDPOINTS" "TAUT_INSECURE_NO_SIDECARS" "TAUT_TOKEN_ALLOW_ENDPOINTS" "TAUT_TOKEN_TENANTS" "TAUT_TOKEN_CLIENTS" "TAUT_TOKEN_CLOUDS" "TAUT_TOKEN_ENV_ALLOW" "TAUT_SECRETS_DIR" "TAUT_PROXY_ADDR" "TAUT_PROXY_ALLOW" "TAUT_ENTRA_AUTHORITY" "TAUT_GRAPH_URL" "TAUT_SLACK_FILES_URL" "TAUT_NATS_URL" "TAUT_NATS_URL_FILE" "TAUT_NATS_CREDS_FILE" "TAUT_NATS_CA_FILE" "TAUT_NATS_CERT_FILE" "TAUT_NATS_KEY_FILE" "TAUT_WORKER_HEALTH_ADDR" "TAUT_CONTROLLER_TOKEN" -}}
{{- range $where, $envs := dict "controller.extraEnv" $v.controller.extraEnv "worker.extraEnv" $v.worker.extraEnv "egressProxy.extraEnv" $v.egressProxy.extraEnv "telemetry.env" $v.telemetry.env -}}
  {{- range $e := $envs -}}
    {{- if has $e.name $forbidden -}}
      {{- fail (printf "\n\n%s may not set %s.\nThat variable is security-relevant and is set only by the chart, from its own values (entra.*, egressProxy.extraAllow, controllerToken.*, or the dev.* toggles together with dev.insecure=true)." $where $e.name) -}}
    {{- end -}}
  {{- end -}}
{{- end -}}

{{/* 3. Native sidecars need Kubernetes 1.29. */}}
{{- if and (include "taut.sidecars" .) (not $v.legacySidecars) (semverCompare "<1.29.0-0" .Capabilities.KubeVersion.Version) -}}
  {{- fail (printf "\n\nThis cluster runs Kubernetes %s. The worker pod uses native sidecars (initContainers with restartPolicy: Always), which need 1.29 or later.\nUpgrade the cluster, or set legacySidecars=true to run the token broker and the egress proxy as ordinary containers." .Capabilities.KubeVersion.Version) -}}
{{- end -}}

{{/* 4. The tenant the token broker is pinned to. */}}
{{- if not $v.entra.tenantId -}}
  {{- fail "\n\nentra.tenantId is required: the directory (tenant) ID of the Microsoft 365 tenant being migrated into.\nThe token broker signs in to this tenant only. Without it, it refuses every sign-in." -}}
{{- end -}}
{{- if not $v.entra.clientId -}}
  {{- fail "\n\nentra.clientId is required: the application (client) ID of the Entra app registration (scripts/entra-app.sh prints it).\nThe token broker signs in as this application only." -}}
{{- end -}}

{{/* 5. Backing services. */}}
{{- if not $v.nats.url -}}
  {{- fail "\n\nnats.url is required, for example nats://nats.nats.svc:4222 (or tls://... for TLS).\nThe egress proxies share the tenant's rate budget over NATS; without it each would take the whole budget for itself." -}}
{{- end -}}
{{- if contains "@" $v.nats.url -}}
  {{- fail "\n\nnats.url contains credentials (user:password@). A value ends up in the pod spec in plain text.\nGive each identity a NATS credentials file instead: put them in a Secret and set nats.credentials.existingSecret (docs/OPERATIONS.md, \"NATS authorization\")." -}}
{{- end -}}
{{- if and $v.nats.tls.existingSecret (not $v.nats.tls.caKey) (not $v.nats.tls.certKey) -}}
  {{- fail "\n\nnats.tls.existingSecret is set but neither nats.tls.caKey nor nats.tls.certKey is. Name the key of the CA bundle, of the client certificate, or both." -}}
{{- end -}}
{{- if and $v.nats.tls.existingSecret $v.nats.tls.certKey (not $v.nats.tls.keyKey) -}}
  {{- fail "\n\nnats.tls.certKey is set without nats.tls.keyKey. A client certificate needs its private key." -}}
{{- end -}}
{{- if not $v.s3.bucket -}}
  {{- fail "\n\ns3.bucket is required: the bucket that holds staging (the encrypted export and batches)." -}}
{{- end -}}
{{- if and $v.s3.endpoint (not (regexMatch "^https?://[^/@]+/?$" $v.s3.endpoint)) -}}
  {{- fail (printf "\n\ns3.endpoint %q must be http(s)://host[:port] with no path and no credentials. Leave it empty to use AWS S3 itself." $v.s3.endpoint) -}}
{{- end -}}

{{/* 6. Every Secret reference resolves (each helper fails with its own message). */}}
{{- $_ := include "taut.postgresSecret" . -}}
{{- $_ := include "taut.stagingKeySecret" . -}}
{{- $_ := include "taut.s3Secret" . -}}
{{- $_ := include "taut.entraSecret" . -}}
{{- if not (and $v.dev.insecure $v.dev.controllerInsecure) -}}
  {{- $_ := include "taut.controllerTokenSecret" . -}}
{{- end -}}
{{- if and $v.secrets.create (not $v.s3.existingSecret) (not $v.secrets.s3SecretKey) -}}
  {{- fail "\n\nsecrets.create is on and s3.existingSecret is empty, but secrets.s3SecretKey is not set." -}}
{{- end -}}

{{/* 7. Autoscaling bounds. */}}
{{- if and $v.worker.autoscaling.enabled (gt (int $v.worker.autoscaling.minReplicas) (int $v.worker.autoscaling.maxReplicas)) -}}
  {{- fail "\n\nworker.autoscaling.minReplicas is greater than worker.autoscaling.maxReplicas." -}}
{{- end -}}
{{- end -}}

{{/* NetworkPolicy --------------------------------------------------------- */}}

{{- /*
taut.npRule (dict "dep" $np.postgres "udp" false "public" $except): one egress
rule for a dependency. A rule without `to` allows every destination, so a
dependency whose `to` list is empty gets one unless "any" is set (DNS): public
IPv4 addresses, with the same ranges left out as for the worker's own
internet egress. Otherwise an unpinned dependency on port 443 would give the
pod the private networks and the cloud metadata service, which is what that
list is there to keep out of reach.
*/ -}}
{{- define "taut.npRule" -}}
- ports:
    {{- range .dep.ports }}
    - protocol: TCP
      port: {{ . }}
    {{- if $.udp }}
    - protocol: UDP
      port: {{ . }}
    {{- end }}
    {{- end }}
  {{- if .dep.to }}
  to:
    {{- toYaml .dep.to | nindent 4 }}
  {{- else if not .any }}
  to:
    - ipBlock:
        cidr: 0.0.0.0/0
        {{- with .public }}
        except:
          {{- toYaml . | nindent 10 }}
        {{- end }}
  {{- end }}
{{- end -}}

{{- /* Egress both kinds of pod need: DNS, Postgres, NATS, the object store, the collector. */ -}}
{{- define "taut.npCommonEgress" -}}
{{- $np := .Values.networkPolicy -}}
{{- $public := $np.workerExternalEgress.except -}}
# DNS
{{ include "taut.npRule" (dict "dep" $np.dns "udp" true "any" true) }}
# Postgres
{{ include "taut.npRule" (dict "dep" $np.postgres "public" $public) }}
# NATS
{{ include "taut.npRule" (dict "dep" $np.nats "public" $public) }}
# S3 staging
{{ include "taut.npRule" (dict "dep" $np.s3 "public" $public) }}
{{- if .Values.telemetry.endpoint }}
# OTLP collector
{{ include "taut.npRule" (dict "dep" $np.otlp "public" $public) }}
{{- end }}
{{- end -}}
