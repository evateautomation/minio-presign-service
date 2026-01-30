#!/bin/sh
set -eu

: "${MINIO_ALIAS:=myminio}"

if [ -n "${MINIO_ENDPOINT:-}" ] && [ -n "${MINIO_ACCESS_KEY:-}" ] && [ -n "${MINIO_SECRET_KEY:-}" ]; then
  echo "Configuring mc alias: ${MINIO_ALIAS} -> ${MINIO_ENDPOINT}"
  mc alias set "${MINIO_ALIAS}" "${MINIO_ENDPOINT}" "${MINIO_ACCESS_KEY}" "${MINIO_SECRET_KEY}" --api S3v4 >/dev/null
else
  echo "WARN: MINIO_ENDPOINT / MINIO_ACCESS_KEY / MINIO_SECRET_KEY not fully set. Assuming alias already exists in container."
fi

exec ./server
