#!/usr/bin/env bash
# Builds the image and starts commandcenter with PostgreSQL, an S3 server
# (SeaweedFS), an Azure Blob emulator (Azurite, answering under the storage
# account's host name with a certificate made for the run) and a directory
# (OpenLDAP), then manages it with the Terraform provider for Nexus
# (datadrivers/nexus, the version test/compat/terraform/main.tf pins) from the
# official OpenTofu image:
#   1. apply: one resource of every kind Command Center serves (repositories
#      of every format and type, blob stores, privileges, roles, a user,
#      realms, SAML, e-mail, ...) and a data source for each
#   2. plan -detailed-exitcode: nothing to change (no drift); the users it made
#      and the directory's users sign in as their settings say
#   3. apply with step=2, which changes attributes of all of them and takes
#      credentials, policies and rules away; no drift; they sign in as the new
#      settings say; back to step 1
#   4. import: every resource is removed from the state and imported again;
#      only the write-only secrets may then differ, and after one apply nothing
#   5. what is left out on purpose (scripts, capabilities, IQ and OpenID Connect
#      in Nexus's shape) fails with the reason
#   6. destroy: nothing of it is left on the server
# Finally prints the container's memory usage.
#   TF_IMAGE       the CLI (default ghcr.io/opentofu/opentofu:1.13.8)
#   S3_IMAGE       the S3 server (default chrislusf/seaweedfs:latest)
#   AZURITE_IMAGE  the Azure Blob emulator (default mcr.microsoft.com/azure-storage/azurite:3.35.0)
#   LDAP_IMAGE     the directory (default bitnamilegacy/openldap:2.6)
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/compat/.work-terraform-$$"
net=commandcenter-terraform-$$
name=commandcenter-terraform-$$
. test/postgres.sh
tfimage="${TF_IMAGE:-ghcr.io/opentofu/opentofu:1.13.8}"
s3image="${S3_IMAGE:-chrislusf/seaweedfs:latest}"
azimage="${AZURITE_IMAGE:-mcr.microsoft.com/azure-storage/azurite:3.35.0}"
ldapimage="${LDAP_IMAGE:-bitnamilegacy/openldap:2.6}"
azhost=devstoreaccount1.blob.core.windows.net
pw=compat-pass

cleanup() {
  docker rm -f -v "$name" "$name-pg" "$name-s3" "$name-azure" "$name-ldap" "$name-tf" >/dev/null 2>&1 || true # -v: the images declare volumes
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work/plugins" "$work/azurite"
cp -r test/compat/terraform "$work/tf"
mkdir -p "$work/tf/certs"
cp internal/pgp/testdata/ed25519.asc "$work/tf/apt-signing.asc"

fail() { echo "FAIL: $*" >&2; exit 1; }

# tf <directory of the work copy> <arguments>: the CLI in its image, as the
# calling user, so that the state and the plugins can be removed afterwards.
tf() {
  local dir="$1"
  shift
  docker run --rm --name "$name-tf" --network "$net" --memory 1g --user "$(id -u):$(id -g)" \
    -e HOME=/tmp -e TF_IN_AUTOMATION=1 -e TF_INPUT=0 -e TF_PLUGIN_CACHE_DIR=/work/plugins \
    -e NEXUS_URL=http://commandcenter:8081 -e NEXUS_USERNAME=admin -e NEXUS_PASSWORD="$pw" \
    -v "$root/$work:/work" -w "/work/tf/$dir" "$tfimage" "$@"
}
# run <what> <tf arguments>: its output is shown only when it fails.
run() {
  local what="$1"
  shift
  if ! tf "$@" >"$work/tf.log" 2>&1; then
    cat "$work/tf.log" >&2
    fail "$what"
  fi
}
# nodrift <what> [plan arguments]: the plan must be empty.
nodrift() {
  local what="$1" rc=0
  shift
  tf . plan -detailed-exitcode -no-color "$@" >"$work/plan.log" 2>&1 || rc=$?
  if [ "$rc" != 0 ]; then
    cat "$work/plan.log" >&2
    fail "$what: the plan is not empty (exit $rc)"
  fi
}
# Requests from inside the network (no port is published): api <path> is a
# GET as the administrator; status <wget arguments> prints ok, or the HTTP
# status of a refusal; anonymous <path> is the status of a GET as nobody.
basic="$(printf 'admin:%s' "$pw" | base64)"
api() { docker exec "$name-pg" wget -q -O - --header "Authorization: Basic $basic" "http://commandcenter:8081$1"; }
status() {
  local out
  if out="$(docker exec "$name-pg" wget -q -O /dev/null "$@" 2>&1)"; then
    echo ok
  else
    echo "$out" | sed -nE 's/.*HTTP\/[0-9.]+ ([0-9]+).*/\1/p'
  fi
}
anonymous() { status "http://commandcenter:8081$1"; }
# signin <user> <password>: the status of what only a holder of the role
# tf-docs may read, the users.
signin() { status --header "Authorization: Basic $(printf '%s:%s' "$1" "$2" | base64)" http://commandcenter:8081/service/rest/v1/security/users; }
wait_ready() {
  for _ in $(seq 30); do
    if [ "$(anonymous /service/rest/v1/status)" = ok ]; then return; fi
    sleep 1
  done
  docker logs "$name" >&2 || true
  fail "commandcenter did not become ready"
}
# expect <want> <got> <what>
expect() { [ "$1" = "$2" ] || fail "$3: want $1, got $2"; }
# has <text> <substring> <what>
has() {
  case "$1" in *"$2"*) ;; *) fail "$3: no $2 in $1" ;; esac
}

docker build -q -t commandcenter:compat-terraform . >/dev/null
hash="$(printf '%s\n' "$pw" | docker run --rm -i --memory 128m commandcenter:compat-terraform hash-password)"
cat > "$work/config.yaml" <<EOF
anonymousRead: false
baseURL: "http://commandcenter:8081"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
EOF
docker network create "$net" >/dev/null

echo "== a certificate for the Azure endpoint"
run "init of the certificate" cert init -no-color
run "the certificate" cert apply -auto-approve -no-color
tf cert output -raw cert > "$work/tf/certs/azure.crt"
tf cert output -raw key > "$work/azurite/azure.key"
cp "$work/tf/certs/azure.crt" "$work/azurite/azure.crt"

echo "== S3 (SeaweedFS) and Azure Blob (Azurite)"
cat > "$work/s3.json" <<'EOF'
{"identities":[{"name":"tf","credentials":[{"accessKey":"tfaccesskey","secretKey":"tfsecretkey0123456789"}],"actions":["Admin","Read","Write","List","Tagging"]}]}
EOF
docker run -d --name "$name-s3" --network "$net" --network-alias s3 --memory 512m \
  -v "$root/$work/s3.json:/etc/seaweedfs/s3.json:ro" "$s3image" \
  server -s3 -s3.port=8333 -s3.config=/etc/seaweedfs/s3.json -volume.max=5 -master.volumeSizeLimitMB=64 >/dev/null
# The account and key are Azurite's published development account.
cat > "$work/azurite/container.js" <<'EOF'
const crypto = require('crypto'), https = require('https');
const account = 'devstoreaccount1', host = account + '.blob.core.windows.net', container = process.argv[2];
const key = Buffer.from('Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==', 'base64');
const date = new Date().toUTCString(), version = '2021-08-06';
const toSign = ['PUT', '', '', '', '', '', '', '', '', '', '', '', 'x-ms-date:' + date, 'x-ms-version:' + version,
  '/' + account + '/' + container, 'restype:container'].join('\n');
const signature = crypto.createHmac('sha256', key).update(toSign, 'utf8').digest('base64');
https.request({ host: '127.0.0.1', port: 443, servername: host, rejectUnauthorized: false, method: 'PUT',
  path: '/' + container + '?restype=container',
  headers: { Host: host, 'x-ms-date': date, 'x-ms-version': version, 'Content-Length': 0, Authorization: 'SharedKey ' + account + ':' + signature } },
  res => process.exit(res.statusCode === 201 || res.statusCode === 409 ? 0 : 1)).on('error', () => process.exit(1)).end();
EOF
docker run -d --name "$name-azure" --network "$net" --network-alias "$azhost" --memory 512m \
  -v "$root/$work/azurite:/certs:ro" "$azimage" \
  azurite-blob --blobHost 0.0.0.0 --blobPort 443 --cert /certs/azure.crt --key /certs/azure.key --inMemoryPersistence --skipApiVersionCheck --loose >/dev/null
bucket=0 container=0
for _ in $(seq 60); do
  if [ "$bucket" = 0 ] && docker exec "$name-s3" sh -c 'echo "s3.bucket.create -name tf-bucket" | weed shell' 2>/dev/null | grep -q 'created bucket'; then
    bucket=1
  fi
  if [ "$container" = 0 ] && docker exec "$name-azure" node /certs/container.js tf-container 2>/dev/null; then
    container=1
  fi
  [ "$bucket$container" = 11 ] && break
  sleep 1
done
if [ "$bucket$container" != 11 ]; then
  docker logs --tail 20 "$name-s3" >&2 || true
  docker logs --tail 20 "$name-azure" >&2 || true
  fail "no bucket (${bucket}) or container (${container}) after 60s"
fi

echo "== the directory (OpenLDAP)"
# The groups are in the users' description attribute: no memberOf overlay.
cat > "$work/seed.ldif" <<'EOF'
dn: dc=example,dc=org
objectClass: dcObject
objectClass: organization
dc: example
o: example

dn: ou=people,dc=example,dc=org
objectClass: organizationalUnit
ou: people

dn: ou=contractors,ou=people,dc=example,dc=org
objectClass: organizationalUnit
ou: contractors

dn: uid=carol,ou=people,dc=example,dc=org
objectClass: inetOrgPerson
uid: carol
cn: Carol Engineer
sn: Engineer
mail: carol@example.org
employeeType: engineer
description: cn=tf-docs,ou=groups,dc=example,dc=org
userPassword: carol-ldap-pw

dn: uid=dave,ou=people,dc=example,dc=org
objectClass: inetOrgPerson
uid: dave
cn: Dave Sales
sn: Sales
employeeType: sales
userPassword: dave-ldap-pw

dn: uid=erin,ou=contractors,ou=people,dc=example,dc=org
objectClass: inetOrgPerson
uid: erin
cn: Erin Contractor
sn: Contractor
employeeType: engineer
description: cn=tf-docs,ou=groups,dc=example,dc=org
userPassword: erin-ldap-pw

dn: uid=mallory,ou=people,dc=example,dc=org
objectClass: inetOrgPerson
uid: mallory
cn: Mallory Admin
sn: Admin
employeeType: engineer
description: cn=admin,ou=groups,dc=example,dc=org
userPassword: mallory-ldap-pw
EOF
chmod 644 "$work/seed.ldif"
docker run -d --name "$name-ldap" --network "$net" --network-alias ldap --memory 256m \
  -e LDAP_ROOT=dc=example,dc=org -e LDAP_ADMIN_USERNAME=admin -e LDAP_ADMIN_PASSWORD=ldap-admin-secret \
  -e LDAP_CUSTOM_LDIF_DIR=/ldifs -v "$root/$work/seed.ldif:/ldifs/seed.ldif:ro" "$ldapimage" >/dev/null
ready=0
for _ in $(seq 60); do
  if docker exec "$name-ldap" ldapsearch -x -H ldap://localhost:1389 -D cn=admin,dc=example,dc=org -w ldap-admin-secret \
    -b ou=people,dc=example,dc=org '(uid=carol)' uid 2>/dev/null | grep -q '^uid: carol'; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  docker logs --tail 30 "$name-ldap" >&2 || true
  fail "the directory was not ready after 60s"
fi

echo "== commandcenter"
start_postgres
# SSL_CERT_DIR: the Azure client trusts the system roots, which the run's
# certificate joins this way.
docker run -d --name "$name" --network "$net" --network-alias commandcenter --memory 128m \
  -e COMMANDCENTER_DATABASE_URL="$pg_url" -e SSL_CERT_DIR=/etc/commandcenter/certs \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" -v "$root/$work/tf/certs:/etc/commandcenter/certs:ro" \
  commandcenter:compat-terraform >/dev/null
wait_ready
# The provider has no resource for cleanup policies: the one the repositories name.
expect ok "$(status --header "Authorization: Basic $basic" --header 'Content-Type: application/json' \
  --post-data '{"name":"tf-cleanup","format":"*","criteriaLastBlobUpdated":30}' http://commandcenter:8081/service/rest/v1/cleanup-policies)" "cleanup policy"

echo "== apply: one of every kind, and no drift"
run "init" . init -no-color
run "the first apply" . apply -auto-approve -no-color
nodrift "after the first apply"
proxy="$(api /service/rest/v1/repositories/maven/proxy/tf-maven-proxy)"
has "$proxy" '"authentication":{"preemptive":true,"type":"username","username":"remote-user"}' "the proxy's credentials"
has "$proxy" '"cleanup":{"policyNames":["tf-cleanup"]}' "the proxy's cleanup policy"
has "$proxy" '"remoteUrl":"https://maven.example.com/maven2"' "the remote as written"
has "$(api /service/rest/v1/blobstores/group/tf-group)" '"softQuota":{"type":"spaceUsedQuota","limit":4000000000}' "the group's soft quota"
# Anonymous access is on; force_basic_auth keeps it off the docker repository.
expect ok "$(anonymous /service/rest/v1/repositories/tf-npm-hosted)" "anonymous read"
expect 401 "$(anonymous /v2/tf-docker-hosted/app/tags/list)" "anonymous pull from a repository with force_basic_auth"
[ "$(anonymous /v2/tf-docker-proxy/app/tags/list)" != 401 ] || fail "anonymous pull from a repository without force_basic_auth: 401"
expect ok "$(signin tf-alice alice-secret-1)" "the user with her role"
expect ok "$(signin tf-bob bob-secret-1)" "the user with the write-only password"
expect ok "$(signin carol carol-ldap-pw)" "a directory user whose group is the role"
expect 401 "$(signin dave dave-ldap-pw)" "a directory user the filter leaves out"
expect 401 "$(signin erin erin-ldap-pw)" "a directory user below the direct children"
expect 403 "$(signin mallory mallory-ldap-pw)" "a directory user in a group named as the administrators' role"

docker restart "$name" >/dev/null
wait_ready
nodrift "after a restart"

echo "== update: step 2, and no drift"
run "the update" . apply -auto-approve -no-color -var step=2
nodrift "after the update" -var step=2
proxy="$(api /service/rest/v1/repositories/maven/proxy/tf-maven-proxy)"
has "$proxy" '"authentication":null' "the proxy's credentials taken away"
has "$proxy" '"cleanup":null' "the proxy's cleanup policy taken away"
has "$proxy" '"routingRule":""' "the proxy's routing rule taken away"
has "$(api /service/rest/v1/blobstores/group/tf-group)" '"softQuota":null' "the group's soft quota taken away"
expect 401 "$(anonymous /service/rest/v1/repositories/tf-npm-hosted)" "anonymous read turned off"
expect 401 "$(signin tf-alice alice-secret-2)" "the user made disabled"
expect ok "$(signin tf-bob bob-secret-2)" "the new write-only password"
expect 401 "$(signin tf-bob bob-secret-1)" "the old write-only password"
expect ok "$(signin carol carol-ldap-pw)" "the directory user after the update"
expect 403 "$(signin dave dave-ldap-pw)" "a directory user without the filter, and without the group"
expect ok "$(signin erin erin-ldap-pw)" "a directory user in the subtree"
run "the way back to step 1" . apply -auto-approve -no-color
nodrift "back at step 1"

echo "== import"
# Every resource but the certificate and the LDAP order (the provider cannot
# import them) leaves the state and comes back through an import block; the
# routing rule through the import command.
ids="$(tf . state list | sed -nE \
  -e 's/^(nexus_repository_([a-z0-9]+)_(hosted|proxy|group)\.[hpg])$/\1 tf-\2-\3/p' \
  -e 's/^(nexus_repository_raw_hosted\.stored\["([a-z0-9]+)"\])$/\1 tf-raw-on-\2/p' \
  -e 's/^(nexus_blobstore_file\.([ab]))$/\1 tf-file-\2/p')
nexus_blobstore_group.g tf-group
nexus_blobstore_s3.s3 tf-s3
nexus_blobstore_azure.azure tf-azure
nexus_security_content_selector.docs tf-docs
nexus_privilege_application.users tf-app-users
nexus_privilege_repository_view.raw tf-view-raw
nexus_privilege_repository_admin.raw tf-admin-raw
nexus_privilege_repository_content_selector.docs tf-selector-docs
nexus_privilege_wildcard.raw tf-wildcard
nexus_security_role.docs tf-docs
nexus_security_role.parent tf-parent
nexus_security_user.alice tf-alice
nexus_security_user.bob tf-bob
nexus_security_ldap.directory tf-ldap
nexus_security_saml.saml saml
nexus_security_realms.active active
nexus_security_anonymous.anonymous anonymous
nexus_security_user_token.tokens golbalUserTokenConfiguration
nexus_mail_config.smtp cfg"
expect 65 "$(echo "$ids" | wc -l | tr -d ' ')" "resources to import"
addresses=(nexus_routing_rule.internal)
while read -r address id; do
  addresses+=("$address")
  printf 'import {\n  to = %s\n  id = "%s"\n}\n' "$address" "$id"
done <<<"$ids" > "$work/tf/imports.tf"
run "state rm" . state rm -no-color "${addresses[@]}"
run "import of the routing rule" . import -no-color nexus_routing_rule.internal tf-internal
tf . plan -no-color >"$work/plan.log" 2>&1 || { cat "$work/plan.log" >&2; fail "plan of the imports"; }
grep -q '^Plan: 65 to import, 0 to add, 8 to change, 0 to destroy\.' "$work/plan.log" || { cat "$work/plan.log" >&2; fail "the imports: not 65 imports and eight changes"; }
# What cannot be read back and so is set again: passwords and keys, and the
# group type, which the provider does not read.
changed="$(sed -nE 's/^  # (.*) will be updated in-place$/\1/p' "$work/plan.log" | sort | tr '\n' ' ')"
expect "nexus_blobstore_azure.azure nexus_blobstore_s3.s3 nexus_mail_config.smtp nexus_repository_apt_hosted.h nexus_repository_maven_proxy.p nexus_security_ldap.directory nexus_security_user.alice nexus_security_user.bob " "$changed" "resources that differ after the import"
if awk '/^  # /{ r = /will be updated in-place$/ } r && /^ +[-+~] +[a-z_]+ += /{ print $2 }' "$work/plan.log" |
  grep -vxE 'password|password_wo|password_wo_version|secret_access_key|account_key|keypair|auth_password|group_type'; then
  cat "$work/plan.log" >&2
  fail "an attribute other than a secret differs after the import"
fi
run "apply of the imports" . apply -auto-approve -no-color
rm "$work/tf/imports.tf"
nodrift "after the imports"

echo "== what is left out says why"
run "init of the left-out resources" leftout init -no-color
if tf leftout apply -auto-approve -no-color >"$work/leftout.log" 2>&1; then
  fail "the left-out resources were applied"
fi
for reason in 'a script would run arbitrary code' 'script privileges have no Command Center equivalent' 'no capabilities' \
  'no IQ Server integration' 'OpenID Connect by discovery'; do
  grep -q "$reason" "$work/leftout.log" || { cat "$work/leftout.log" >&2; fail "no \"$reason\" in the errors"; }
done

echo "== destroy"
run "destroy" . destroy -auto-approve -no-color
expect '[]' "$(api /service/rest/v1/repositories)" "repositories left"
expect '[]' "$(api /service/rest/v1/routing-rules)" "routing rules left"
expect '[]' "$(api /service/rest/v1/security/content-selectors)" "content selectors left"
expect '[]' "$(api /service/rest/v1/security/ssl/truststore)" "certificates left"
expect '[]' "$(api /service/rest/v1/security/ldap)" "LDAP servers left"
expect '"name":"default"' "$(api /service/rest/v1/blobstores | grep -oE '"name":"[^"]*"' | tr '\n' ' ' | sed 's/ $//')" "blob stores left"
for list in security/users security/roles security/privileges; do
  if api "/service/rest/v1/$list" | grep -q '"tf-'; then
    fail "$list: something of the test is left"
  fi
done
expect 404 "$(status --header "Authorization: Basic $basic" http://commandcenter:8081/service/rest/v1/security/saml)" "the SAML settings after the destroy"
has "$(api /service/rest/v1/email)" '"host":""' "the e-mail settings after the destroy"

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "TERRAFORM COMPAT OK"
