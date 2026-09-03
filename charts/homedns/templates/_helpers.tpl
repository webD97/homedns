{{- define "homedns.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "homedns.fullname" -}}
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

{{- define "homedns.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "homedns.labels" -}}
helm.sh/chart: {{ include "homedns.chart" . }}
{{ include "homedns.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "homedns.selectorLabels" -}}
app.kubernetes.io/name: {{ include "homedns.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "homedns.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "homedns.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* True when the Gateway API integration is on and RESOURCE is watched. */}}
{{- define "homedns.watches" -}}
{{- $resource := .resource -}}
{{- $root := .root -}}
{{- if $root.Values.gatewayAPI.enabled -}}
{{- if has $resource $root.Values.gatewayAPI.resources -}}true{{- end -}}
{{- end -}}
{{- end }}

{{- define "homedns.watchesGatewayAPI" -}}
{{- $root := . -}}
{{- if $root.Values.gatewayAPI.enabled -}}
{{- range $r := list "HTTPRoute" "TLSRoute" "GRPCRoute" -}}
{{- if has $r $root.Values.gatewayAPI.resources -}}true{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/* The pod selector peercache discovers siblings with. Kept in step with
selectorLabels by construction: these are the labels the Deployment selects on. */}}
{{- define "homedns.peerSelector" -}}
app.kubernetes.io/name={{ include "homedns.name" . }},app.kubernetes.io/instance={{ .Release.Name }}
{{- end }}

{{/* Plugin order here is irrelevant; CoreDNS orders the chain itself. */}}
{{- define "homedns.corefile" -}}
{{- if .Values.corefile -}}
{{ .Values.corefile }}
{{- else -}}
.:{{ .Values.service.port }} {
    errors
{{- if .Values.log }}
    log
{{- end }}

    health :8080
    ready :8181
{{- if .Values.metrics.enabled }}
    prometheus 0.0.0.0:{{ .Values.metrics.port }}
{{- end }}

{{- if and .Values.blocklist.enabled .Values.blocklist.urls }}

    blocklist {
{{- range .Values.blocklist.urls }}
        url {{ . }}
{{- end }}
        bootstrap_dns {{ join " " .Values.blocklist.bootstrapDNS }}
{{- if .Values.blocklist.allow }}
        allow {{ join " " .Values.blocklist.allow }}
{{- end }}
        refresh {{ .Values.blocklist.refresh }}
        ready_timeout {{ .Values.blocklist.readyTimeout }}
    }
{{- end }}

{{- if .Values.hosts }}

    hosts {
{{- range .Values.hosts }}
        {{ .ip }} {{ join " " .names }}
{{- end }}
        fallthrough
    }
{{- end }}

{{- if .Values.gatewayAPI.enabled }}

    k8s_gateway {{ join " " .Values.gatewayAPI.zones }} {
        resources {{ join " " .Values.gatewayAPI.resources }}
{{- if .Values.gatewayAPI.gatewayClasses }}
        gatewayClasses {{ join " " .Values.gatewayAPI.gatewayClasses }}
{{- end }}
        ttl {{ .Values.gatewayAPI.ttl }}
{{- if .Values.gatewayAPI.fallthrough }}
        fallthrough
{{- end }}
    }
{{- end }}

{{- if .Values.cache.enabled }}

    cache {{ .Values.cache.ttl }}
{{- end }}

{{- if .Values.peerCache.enabled }}

    peercache {
        selector {{ include "homedns.peerSelector" . }}
        port {{ .Values.peerCache.port }}
    }
{{- end }}

{{- if .Values.race.enabled }}

    race . {{ join " " .Values.upstream.servers }} {
{{- if .Values.upstream.tlsServername }}
        tls_servername {{ .Values.upstream.tlsServername }}
{{- end }}
        expire {{ .Values.race.expire }}
    }
{{- else }}

    forward . {{ join " " .Values.upstream.servers }}{{ if .Values.upstream.tlsServername }} {
        tls_servername {{ .Values.upstream.tlsServername }}
    }{{ end }}
{{- end }}

    loop
    reload
    loadbalance
}
{{- end -}}
{{- end }}
