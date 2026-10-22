#!/bin/bash
#
# The start script of the service.
#
# The service needs the credentials of the bucket the builds are in, and it
# hands them to the commands it runs. They are read here rather than in the
# environment of the service, so that they are not visible to everything that
# runs on the host.
set -e

set -a
. /srv/webhook/credentials.env
set +a

# The commands are run with the credentials of the bucket under the names the
# AWS command line tool expects.
export AWS_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID"
export AWS_SECRET_ACCESS_KEY="$R2_SECRET_ACCESS_KEY"
export AWS_DEFAULT_REGION=auto

# The address of the bucket, so that a deployment does not have to repeat it.
export R2_ENDPOINT="https://${R2_ACCOUNT_ID}.r2.cloudflarestorage.com"

# The secret the deployment API is closed behind. The API is reachable from
# the outside, and what it serves says which applications the host runs, so
# it answers nobody who does not present it. When it is not set, the API is
# closed for everyone.
export DEPLOYMENT_API_TOKEN="${DEPLOYMENT_API_TOKEN:?the deployment API needs a token}"

cd "$(dirname "$0")"

# The service serves the ordinary hooks of the applications that are still
# deployed by their own script, and the deployments of the applications that
# are deployed by the tool itself.
exec ./webhook4deploy \
  -hooks /srv/webhook/hooks.json \
  -hooks /srv/webhook/hooks.deploy.yaml \
  -state-dir /srv/webhook/deployments \
  -app-home "$HOME" \
  -port "${WEBHOOK_PORT:-9000}" \
  -verbose
