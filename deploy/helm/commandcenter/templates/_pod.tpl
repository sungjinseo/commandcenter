{{/*
The pod template of the StatefulSet (replicaCount 1) and of the Deployment (more): the same pod,
whose data volume is the StatefulSet's claim template or the one claim all pods share.
*/}}
{{- define "commandcenter.podTemplate" -}}
template:
  metadata:
    labels:
      {{- include "commandcenter.selectorLabels" . | nindent 6 }}
      {{- with .Values.podLabels }}
      {{- toYaml . | nindent 6 }}
      {{- end }}
    {{- if or (not .Values.config.existingSecret) .Values.podAnnotations }}
    annotations:
      {{- if not .Values.config.existingSecret }}
      # Only the keys read on every start: the imported ones are ignored after the first start.
      checksum/config: {{ include "commandcenter.configChecksum" . }}
      {{- end }}
      {{- with .Values.podAnnotations }}
      {{- toYaml . | nindent 6 }}
      {{- end }}
    {{- end }}
  spec:
    serviceAccountName: {{ include "commandcenter.serviceAccountName" . }}
    automountServiceAccountToken: false
    terminationGracePeriodSeconds: {{ .Values.terminationGracePeriodSeconds }}
    {{- with .Values.imagePullSecrets }}
    imagePullSecrets:
      {{- toYaml . | nindent 6 }}
    {{- end }}
    {{- with .Values.priorityClassName }}
    priorityClassName: {{ . }}
    {{- end }}
    securityContext:
      {{- toYaml .Values.podSecurityContext | nindent 6 }}
    containers:
      - name: commandcenter
        image: {{ include "commandcenter.image" . | quote }}
        imagePullPolicy: {{ .Values.image.pullPolicy }}
        ports:
          - name: http
            containerPort: 8081
        env:
          {{- with .Values.goMemLimit }}
          - name: GOMEMLIMIT
            value: {{ . | quote }}
          {{- end }}
          {{- with .Values.proxy.http }}
          - name: HTTP_PROXY
            value: {{ . | quote }}
          {{- end }}
          {{- with .Values.proxy.https }}
          - name: HTTPS_PROXY
            value: {{ . | quote }}
          {{- end }}
          {{- with .Values.proxy.noProxy }}
          - name: NO_PROXY
            value: {{ . | quote }}
          {{- end }}
          {{- if .Values.secretKey.existingSecret }}
          - name: COMMANDCENTER_SECRET_KEY
            valueFrom:
              secretKeyRef:
                name: {{ .Values.secretKey.existingSecret }}
                key: {{ .Values.secretKey.key }}
          {{- end }}
          {{- if .Values.database.existingSecret }}
          - name: COMMANDCENTER_DATABASE_URL
            valueFrom:
              secretKeyRef:
                name: {{ .Values.database.existingSecret }}
                key: {{ .Values.database.key }}
          {{- end }}
          {{- with .Values.extraEnv }}
          {{- toYaml . | nindent 10 }}
          {{- end }}
        resources:
          {{- toYaml .Values.resources | nindent 10 }}
        securityContext:
          {{- toYaml .Values.securityContext | nindent 10 }}
        {{- with .Values.startupProbe }}
        startupProbe:
          {{- toYaml . | nindent 10 }}
        {{- end }}
        {{- with .Values.readinessProbe }}
        readinessProbe:
          {{- toYaml . | nindent 10 }}
        {{- end }}
        {{- with .Values.livenessProbe }}
        livenessProbe:
          {{- toYaml . | nindent 10 }}
        {{- end }}
        volumeMounts:
          - name: data
            mountPath: /data
          - name: config
            mountPath: /etc/commandcenter
            readOnly: true
          # SQLite's temporary files; the root filesystem is read-only. Never a blob store.
          - name: tmp
            mountPath: /tmp
          {{- with .Values.extraVolumeMounts }}
          {{- toYaml . | nindent 10 }}
          {{- end }}
    volumes:
      {{- with include "commandcenter.claim" . }}
      - name: data
        persistentVolumeClaim:
          claimName: {{ . }}
      {{- end }}
      - name: config
        secret:
          secretName: {{ include "commandcenter.configSecret" . }}
          items:
            - key: config.yaml
              path: config.yaml
      - name: tmp
        emptyDir: {}
      {{- with .Values.extraVolumes }}
      {{- toYaml . | nindent 6 }}
      {{- end }}
    {{- with .Values.nodeSelector }}
    nodeSelector:
      {{- toYaml . | nindent 6 }}
    {{- end }}
    {{- with include "commandcenter.affinity" . }}
    affinity:
      {{- . | nindent 6 }}
    {{- end }}
    {{- with .Values.tolerations }}
    tolerations:
      {{- toYaml . | nindent 6 }}
    {{- end }}
{{- end }}
