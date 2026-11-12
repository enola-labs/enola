{{- define "shop.serviceAccountName" -}}
{{- default (include "common.names.fullname" .) .Values.serviceAccount.name -}}
{{- end -}}
