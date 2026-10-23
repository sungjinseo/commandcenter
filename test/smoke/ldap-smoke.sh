#!/usr/bin/env bash
# Builds the image, starts an OpenLDAP server (one user, alice, in the group
# cc-admins) and commandcenter on one docker network, then configures LDAP
# through the API and checks: lockout protection, the diagnostic test, LDAP
# login through /-/api/login and Basic auth, LDAP-owned tokens, a wrong
# password, the self-service password refusal and turning the local realm
# off and on. Finally prints the container's memory usage.
#
# The image is bitnamilegacy/openldap: Bitnami no longer publishes
# bitnami/openldap tags on Docker Hub. Its memberOf overlay is not enabled
# by default; the script enables it through cn=config before adding the
# group.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-ldap-$$"
net=commandcenter-ldap-smoke-$$
name=commandcenter-ldap-smoke-$$
ldapname=commandcenter-ldap-smoke-dir-$$
ldapimage=bitnamilegacy/openldap:2.6
base=http://localhost:18087
ldapadmin=cn=admin,dc=example,dc=org
ldappw=ldap-admin-Secret-7
alicepw=alice-ldap-pw-9

cleanup() {
  docker rm -f "$name" "$ldapname" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work"

fail() { echo "FAIL: $*" >&2; exit 1; }
# expect <want> <got> <what>
expect() { [ "$1" = "$2" ] || fail "$3: want $1, got $2"; }
# nosecret <text> <what>: the bind and user passwords never appear in output.
nosecret() {
  case "$1" in *"$ldappw"*|*"$alicepw"*) fail "$2 contains a password: $1" ;; esac
}

docker build -q -t commandcenter:smoke . >/dev/null
hash="$(printf 'ldap-smoke-pass\n' | docker run --rm -i commandcenter:smoke hash-password)"

cat > "$work/config.yaml" <<EOF2
anonymousRead: false
baseURL: "http://localhost:18087"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
repositories:
  - { name: maven-releases, format: maven, type: hosted }
EOF2

cat > "$work/seed.ldif" <<'EOF2'
dn: dc=example,dc=org
objectClass: dcObject
objectClass: organization
dc: example
o: example

dn: ou=people,dc=example,dc=org
objectClass: organizationalUnit
ou: people

dn: ou=groups,dc=example,dc=org
objectClass: organizationalUnit
ou: groups

dn: uid=alice,ou=people,dc=example,dc=org
objectClass: inetOrgPerson
uid: alice
cn: Alice Liddell
sn: Liddell
mail: alice@example.org
userPassword: alice-ldap-pw-9
EOF2

docker network create "$net" >/dev/null

echo "== openldap"
docker run -d --name "$ldapname" --network "$net" --memory 256m \
  -e LDAP_ROOT=dc=example,dc=org -e LDAP_ADMIN_USERNAME=admin -e LDAP_ADMIN_PASSWORD="$ldappw" \
  -e LDAP_CONFIG_ADMIN_ENABLED=yes -e LDAP_CONFIG_ADMIN_USERNAME=config -e LDAP_CONFIG_ADMIN_PASSWORD=config-pw \
  -e LDAP_CUSTOM_LDIF_DIR=/ldifs -v "$root/$work/seed.ldif:/ldifs/seed.ldif:ro" "$ldapimage" >/dev/null
ready=0
for i in $(seq 1 60); do
  if docker exec "$ldapname" ldapsearch -x -H ldap://localhost:1389 -D "$ldapadmin" -w "$ldappw" \
      -b ou=people,dc=example,dc=org "(uid=alice)" uid 2>/dev/null | grep -q '^uid: alice'; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  docker logs "$ldapname" >&2 || true
  fail "openldap not ready after 60s"
fi
# memberOf, then the group (memberOf is maintained when the group is written).
docker exec -i "$ldapname" ldapmodify -x -H ldap://localhost:1389 -D cn=config,cn=config -w config-pw >/dev/null <<'EOF2'
dn: cn=module{0},cn=config
changetype: modify
add: olcModuleLoad
olcModuleLoad: /opt/bitnami/openldap/lib/openldap/memberof.so

dn: olcOverlay=memberof,olcDatabase={2}mdb,cn=config
changetype: add
objectClass: olcOverlayConfig
objectClass: olcMemberOf
olcOverlay: memberof
EOF2
docker exec -i "$ldapname" ldapadd -x -H ldap://localhost:1389 -D "$ldapadmin" -w "$ldappw" >/dev/null <<'EOF2'
dn: cn=cc-admins,ou=groups,dc=example,dc=org
objectClass: groupOfNames
cn: cc-admins
member: uid=alice,ou=people,dc=example,dc=org
EOF2
docker exec "$ldapname" ldapsearch -x -H ldap://localhost:1389 -D "$ldapadmin" -w "$ldappw" \
  -b ou=people,dc=example,dc=org "(uid=alice)" memberOf | grep -qi '^memberOf: cn=cc-admins' || fail "memberOf not set"

echo "== commandcenter"
docker run -d --name "$name" --network "$net" --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" -p 18087:8081 commandcenter:smoke >/dev/null
ready=0
for i in $(seq 1 30); do
  if curl -fs "$base/service/rest/v1/status" >/dev/null; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  docker logs "$name" >&2 || true
  fail "commandcenter not ready after 30s"
fi

jar="$work/cookies.txt"
ui=(-H 'X-Command-Center-UI: 1')
adm=(-u admin:ldap-smoke-pass)
status() { curl -s -o /dev/null -w '%{http_code}' "$@" || true; }

echo "== lockout: local off without an LDAP admin mapping"
expect 409 "$(status "${adm[@]}" "${ui[@]}" -X PUT -d '{"realms":[{"id":"local","enabled":false},{"id":"token","enabled":true}]}' "$base/-/api/realms")" "local off, no LDAP"
expect 400 "$(status "${adm[@]}" "${ui[@]}" -X PUT -d '{"realms":[{"id":"local","enabled":true},{"id":"ldap","enabled":true}]}' "$base/-/api/realms")" "ldap on before it is configured"

echo "== configure LDAP"
ldapcfg() { # ldapcfg <groupMappings JSON>
  printf '{"url":"ldap://%s:1389","bindDN":"%s","bindPassword":"%s","userBaseDN":"ou=people,dc=example,dc=org","userFilter":"(&(objectClass=inetOrgPerson)(uid={user}))","groupMappings":%s}' \
    "$ldapname" "$ldapadmin" "$ldappw" "$1"
}
resp="$(curl -s -w ' HTTP=%{http_code}' "${adm[@]}" "${ui[@]}" -X PUT -d "$(ldapcfg '[{"group":"cc-admins","roles":["admin"]}]')" "$base/-/api/realms/ldap")"
case "$resp" in *" HTTP=200") ;; *) fail "LDAP PUT: $resp" ;; esac
nosecret "$resp" "LDAP PUT response"
echo "$resp" | grep -q '"bindPasswordSet":true' || fail "bindPasswordSet: $resp"
got="$(curl -s "${adm[@]}" "$base/-/api/realms/ldap")"
nosecret "$got" "LDAP GET"
expect 200 "$(status "${adm[@]}" "${ui[@]}" -X PUT -d '{"realms":[{"id":"local","enabled":true},{"id":"token","enabled":true},{"id":"ldap","enabled":true}]}' "$base/-/api/realms")" "enable ldap"

echo "== diagnostic test"
diag="$(curl -s "${adm[@]}" "${ui[@]}" -X POST -d "{\"user\":\"alice\",\"password\":\"$alicepw\"}" "$base/-/api/realms/ldap/test")"
nosecret "$diag" "test output"
echo "$diag" | grep -q '"ok":true' || fail "test: $diag"
for step in connect bind search userBind groups; do
  echo "$diag" | grep -q "\"step\":\"$step\",\"ok\":true" || fail "step $step: $diag"
done
echo "$diag" | grep -q 'cc-admins' || fail "groups step lacks the group: $diag"

echo "== LDAP login"
expect 401 "$(status "${ui[@]}" -X POST -d '{"user":"alice","password":"wrong"}' "$base/-/api/login")" "wrong LDAP password"
expect 401 "$(status "${ui[@]}" -X POST -d '{"user":"alice","password":""}' "$base/-/api/login")" "empty LDAP password"
expect 401 "$(status "${ui[@]}" -X POST -d "{\"user\":\"*\",\"password\":\"$alicepw\"}" "$base/-/api/login")" "filter injection"
expect 200 "$(status -c "$jar" "${ui[@]}" -X POST -d "{\"user\":\"alice\",\"password\":\"$alicepw\"}" "$base/-/api/login")" "LDAP login"
me="$(curl -s -b "$jar" "$base/-/api/me")"
echo "$me" | grep -q '"user":"alice"' || fail "me: $me"
echo "$me" | grep -q '"realm":"LDAP"' || fail "me realm: $me"
echo "$me" | grep -q '"admin":true' || fail "me admin (cc-admins -> admin): $me"
expect 400 "$(status -b "$jar" "${ui[@]}" -X PUT -d "{\"currentPassword\":\"$alicepw\",\"newPassword\":\"another-password\"}" "$base/-/api/account/password")" "own password of an LDAP user"

echo "== Basic auth to a repository"
printf '<project>ldap-smoke</project>\n' > "$work/hello-1.0.0.pom"
url="$base/repository/maven-releases/com/example/hello/1.0.0/hello-1.0.0.pom"
expect 201 "$(status -u "alice:$alicepw" -X PUT --data-binary @"$work/hello-1.0.0.pom" "$url")" "maven PUT as LDAP alice"
expect 200 "$(status -u "alice:$alicepw" "$url")" "maven GET as LDAP alice"
expect 401 "$(status -u "alice:wrong" "$url")" "Basic with a wrong LDAP password"

echo "== LDAP-owned token"
resp="$(curl -s -b "$jar" "${ui[@]}" -X POST -d '{}' "$base/-/api/tokens")"
token="$(echo "$resp" | sed -nE 's/.*"token":"([^"]+)".*/\1/p')"
[ -n "$token" ] || fail "token create: $resp"
echo "$resp" | grep -q '"user":"ldap:alice"' || fail "token owner: $resp"
expect 200 "$(status -H "Authorization: Bearer $token" "$url")" "LDAP token as Bearer"
expect 200 "$(status -u "alice:$token" "$url")" "LDAP token as Basic password"
expect 401 "$(status -u "admin:$token" "$url")" "LDAP token with another user name"
curl -s -b "$jar" "$base/-/api/tokens" | grep -q '"user":"ldap:alice"' || fail "alice's token list"

echo "== realms: local off with an LDAP admin mapping, then the mapping cannot go"
expect 200 "$(status -b "$jar" "${ui[@]}" -X PUT -d '{"realms":[{"id":"ldap","enabled":true},{"id":"local","enabled":false},{"id":"token","enabled":true}]}' "$base/-/api/realms")" "local off with an LDAP admin mapping"
expect 401 "$(status "${adm[@]}" "$base/-/api/me")" "local Basic with the local realm off"
expect 409 "$(status -b "$jar" "${ui[@]}" -X PUT -d "$(ldapcfg '[]')" "$base/-/api/realms/ldap")" "removing the last LDAP admin mapping with local off"
expect 200 "$(status -b "$jar" "${ui[@]}" -X PUT -d '{"realms":[{"id":"local","enabled":true},{"id":"token","enabled":true},{"id":"ldap","enabled":true}]}' "$base/-/api/realms")" "local back on"
expect 200 "$(status "${adm[@]}" "$base/-/api/me")" "local admin again"

echo "== logs carry no secret"
logs="$(docker logs "$name" 2>&1)"
nosecret "$logs" "server log"

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "LDAP SMOKE OK"
