#!/usr/bin/env bash
# Builds the image, starts an OpenLDAP server (one user, alice, in the group
# cc-admins) and commandcenter on one docker network, then configures LDAP
# through the API and checks: lockout protection, the diagnostic test, LDAP
# login through /-/api/login and Basic auth, LDAP-owned tokens, a wrong
# password, the self-service password refusal and turning the local realm
# off and on.
#
# Then static groups, where the group entries list their members and no user
# has them in an attribute: groupOfNames with member as a DN (${dn}, one level
# and the subtree, a group of 20000 members), posixGroup with memberUid as a
# plain name (${username}), groupOfUniqueNames with a DN made of the name. A
# member gets a role that really lets it upload, by the group's name and by a
# mapping; a user outside the group does not, and a group named as the admin
# role makes no admin. Users whose id would widen the search find no group. A
# search that fails signs the user in without those roles. The Nexus API takes
# the same settings and reads them back. A member taken out loses the role at
# the next re-check (about a minute of the run is that wait). Finally prints
# the container's memory usage.
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
. test/postgres.sh
ldapname=commandcenter-ldap-smoke-dir-$$
ldapimage=bitnamilegacy/openldap:2.6
base=http://localhost:18087
ldapadmin=cn=admin,dc=example,dc=org
ldappw=ldap-admin-Secret-7
alicepw=alice-ldap-pw-9
image=commandcenter:smoke-ldap

cleanup() {
  docker rm -f -v "$name" "$name-pg" "$ldapname" >/dev/null 2>&1 || true
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

docker build -q -t "$image" . >/dev/null
hash="$(printf 'ldap-smoke-pass\n' | docker run --rm -i "$image" hash-password)"

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

dn: ou=sub,ou=groups,dc=example,dc=org
objectClass: organizationalUnit
ou: sub

dn: cn=deep,ou=sub,ou=groups,dc=example,dc=org
objectClass: groupOfNames
cn: deep
member: uid=erin,ou=people,dc=example,dc=org

dn: cn=admin,ou=groups,dc=example,dc=org
objectClass: groupOfNames
cn: admin
member: uid=mallory,ou=people,dc=example,dc=org

dn: cn=cc-posix,ou=groups,dc=example,dc=org
objectClass: posixGroup
cn: cc-posix
gidNumber: 5000
memberUid: carol

dn: cn=cc-uniq,ou=groups,dc=example,dc=org
objectClass: groupOfUniqueNames
cn: cc-uniq
uniqueMember: uid=erin,ou=people,dc=example,dc=org

EOF2
for u in bob carol dave erin mallory; do
  printf 'dn: uid=%s,ou=people,dc=example,dc=org\nobjectClass: inetOrgPerson\nuid: %s\ncn: %s\nsn: %s\nuserPassword: %s-ldap-pw-9\n\n' "$u" "$u" "$u" "$u" "$u"
done >> "$work/seed.ldif"
# Users whose id would widen a group search that did not escape it.
cat >> "$work/seed.ldif" <<'EOF2'
dn: uid=\2A,ou=people,dc=example,dc=org
objectClass: inetOrgPerson
uid: *
cn: star
sn: star
userPassword: star-ldap-pw-9

dn: uid=\2A\29\28cn\3D\2A,ou=people,dc=example,dc=org
objectClass: inetOrgPerson
uid: *)(cn=*
cn: breakout
sn: breakout
userPassword: breakout-ldap-pw-9

EOF2
{
  printf 'dn: cn=cc-devs,ou=groups,dc=example,dc=org\nobjectClass: groupOfNames\ncn: cc-devs\nmember: uid=bob,ou=people,dc=example,dc=org\n'
  seq 1 20000 | sed 's/.*/member: uid=u&,ou=people,dc=example,dc=org/'
  printf '\n'
} >> "$work/seed.ldif"

docker network create "$net" >/dev/null

echo "== openldap"
docker run -d --name "$ldapname" --network "$net" --memory 256m \
  -e LDAP_ROOT=dc=example,dc=org -e LDAP_ADMIN_USERNAME=admin -e LDAP_ADMIN_PASSWORD="$ldappw" \
  -e LDAP_CONFIG_ADMIN_ENABLED=yes -e LDAP_CONFIG_ADMIN_USERNAME=config -e LDAP_CONFIG_ADMIN_PASSWORD=config-pw \
  -e LDAP_CUSTOM_LDIF_DIR=/ldifs -v "$root/$work/seed.ldif:/ldifs/seed.ldif:ro" "$ldapimage" >/dev/null
# The image loads the seed through a server it then stops: the one to wait for
# is the one that logs this.
ready=0
for i in $(seq 1 60); do
  if docker logs "$ldapname" 2>&1 | grep -q 'slapd starting' && docker exec "$ldapname" ldapsearch -x -H ldap://localhost:1389 -D "$ldapadmin" -w "$ldappw" \
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
start_postgres
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" -p 127.0.0.1:18087:8081 "$image" >/dev/null
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
printf '<project><groupId>com.example</groupId><artifactId>hello</artifactId><version>1.0.0</version></project>\n' > "$work/hello-1.0.0.pom"
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

echo "== static groups: without them, these users have no role"
# writes <curl credentials>: the status of a new upload with them.
writes() {
  local id
  id="s$(date +%s%N)"
  printf '<project><groupId>com.example</groupId><artifactId>%s</artifactId><version>1.0.0</version></project>\n' "$id" > "$work/$id.pom"
  status "$@" -X PUT --data-binary @"$work/$id.pom" "$base/repository/maven-releases/com/example/$id/1.0.0/$id-1.0.0.pom"
}
# as <user>: the user's /-/api/me.
as() { curl -s -u "$1:$1-ldap-pw-9" "$base/-/api/me"; }
has() { case "$1" in *"$2"*) ;; *) fail "$3: no $2 in $1" ;; esac; }
hasnot() { case "$1" in *"$2"*) fail "$3: $2 in $1" ;; esac; }
for role in cc-devs cc-posix cc-uniq deep; do
  expect 201 "$(status "${adm[@]}" "${ui[@]}" -X POST -d "{\"id\":\"$role\",\"privileges\":[{\"target\":\"repo:maven-releases\",\"actions\":[\"browse\",\"read\",\"add\"]}]}" "$base/-/api/roles")" "role $role"
done
expect 403 "$(writes -u bob:bob-ldap-pw-9)" "bob with groups from his attribute"
expect 403 "$(writes -u carol:carol-ldap-pw-9)" "carol with groups from her attribute"

# staticcfg <class> <member attribute> <member format> <mappings JSON> [<subtree> [<base>]]
staticcfg() {
  printf '{"url":"ldap://%s:1389","bindDN":"%s","bindPassword":"%s","userBaseDN":"ou=people,dc=example,dc=org","userFilter":"(&(objectClass=inetOrgPerson)(uid={user}))",' "$ldapname" "$ldapadmin" "$ldappw"
  printf '"groupMappings":%s,"groupsAsRoles":true,"groupType":"static","groupBaseDN":"%s","groupSubtree":%s,"groupObjectClass":"%s","groupIdAttr":"cn","groupMemberAttr":"%s","groupMemberFormat":"%s"}' \
    "$4" "${6:-ou=groups,dc=example,dc=org}" "${5:-false}" "$1" "$2" "$3"
}
use() { expect 200 "$(status "${adm[@]}" "${ui[@]}" -X PUT -d "$1" "$base/-/api/realms/ldap")" "$2"; }
# diagnose <user> <password>: the diagnostic test's answer, without a password.
diagnose() {
  local out
  out="$(curl -s "${adm[@]}" "${ui[@]}" -X POST -d "{\"user\":\"$1\",\"password\":\"$2\"}" "$base/-/api/realms/ldap/test")"
  nosecret "$out" "test output"
  printf '%s' "$out"
}

echo "== groupOfNames, member as a DN"
use "$(staticcfg groupOfNames member '${dn}' '[{"group":"cn=cc-admins,ou=groups,dc=example,dc=org","roles":["admin"]}]')" "static groups"
got="$(curl -s "${adm[@]}" "$base/-/api/realms/ldap")"
has "$got" '"groupType":"static"' "the settings"
has "$got" '"groupMemberFormat":"${dn}"' "the settings"
diag="$(diagnose bob bob-ldap-pw-9)"
has "$diag" '"ok":true' "the test of bob"
has "$diag" 'found under ou=groups,dc=example,dc=org: [cc-devs, cn=cc-devs,' "the test of bob"
expect 201 "$(writes -u bob:bob-ldap-pw-9)" "bob, in a group of 20000 members, with the role of its name"
got="$(staticcfg groupOfNames member '${dn}' '[]' | sed 's/"bindDN":"[^"]*","bindPassword":"[^"]*"/"bindDN":"","clearBindPassword":true/')"
use "$got" "no service account"
expect 201 "$(writes -u bob:bob-ldap-pw-9)" "bob, the group entries read anonymously after his bind"
use "$(staticcfg groupOfNames member '${dn}' '[{"group":"cn=cc-admins,ou=groups,dc=example,dc=org","roles":["admin"]}]')" "the service account again"
# From here on the group entries are for the service account alone: a search
# that ran as the user who signed in would find nothing.
docker exec -i "$ldapname" ldapmodify -x -H ldap://localhost:1389 -D cn=config,cn=config -w config-pw >/dev/null <<'EOF2'
dn: olcDatabase={2}mdb,cn=config
changetype: modify
add: olcAccess
olcAccess: {0}to dn.subtree="ou=groups,dc=example,dc=org" by * none
olcAccess: {1}to * by * read
EOF2
[ -z "$(docker exec "$ldapname" ldapsearch -x -LLL -H ldap://localhost:1389 -D uid=bob,ou=people,dc=example,dc=org -w bob-ldap-pw-9 -b dc=example,dc=org '(cn=cc-devs)' cn 2>/dev/null)" ] || fail "bob can read the group"
expect 201 "$(writes -u bob:bob-ldap-pw-9)" "bob, the group entries read as the service account"
got="$(as bob)"
has "$got" '"cc-devs"' "bob's roles"
has "$got" '"admin":false' "bob"
expect 200 "$(status -u dave:dave-ldap-pw-9 "$base/-/api/me")" "dave signs in"
expect 403 "$(writes -u dave:dave-ldap-pw-9)" "dave, in no group"
hasnot "$(as dave)" '"cc-' "dave's roles"
expect 403 "$(writes -u carol:carol-ldap-pw-9)" "carol, in a group of another class"
got="$(curl -s -u "alice:$alicepw" "$base/-/api/me")"
has "$got" '"admin":true' "alice, by a mapping to the group's DN"
got="$(as mallory)"
has "$got" '"admin":false' "mallory, in a group named as the admin role"
hasnot "$got" '"roles":["admin"' "mallory's roles"
expect 403 "$(writes -u mallory:mallory-ldap-pw-9)" "mallory, in a group named as the admin role"
expect 403 "$(writes -u erin:erin-ldap-pw-9)" "erin, in a group below the direct children"
use "$(staticcfg groupOfNames member '${dn}' '[]' true)" "the subtree"
expect 201 "$(writes -u erin:erin-ldap-pw-9)" "erin, through the subtree"

echo "== posixGroup, memberUid as a plain name"
use "$(staticcfg posixGroup memberUid '${username}' '[{"group":"cc-admins","roles":["admin"]}]')" "posix groups"
expect 201 "$(writes -u carol:carol-ldap-pw-9)" "carol, in the posix group"
has "$(as carol)" '"cc-posix"' "carol's roles"
# memberUid matches exactly: the directory's value goes in, not what was typed.
expect 201 "$(writes -u CAROL:carol-ldap-pw-9)" "carol, signing in as CAROL"
expect 403 "$(writes -u bob:bob-ldap-pw-9)" "bob, in no posix group"
got="$(curl -s -u "alice:$alicepw" "$base/-/api/me")"
has "$got" '"admin":false' "alice, with cc-admins in memberOf only: an attribute says nothing here"

echo "== a group search cannot be widened"
for u in '*:star' '*)(cn=*:breakout'; do
  diag="$(diagnose "${u%:*}" "${u##*:}-ldap-pw-9")"
  has "$diag" '"step":"userBind","ok":true' "the test of ${u%:*}"
  has "$diag" '"step":"groups","ok":true' "the test of ${u%:*}"
  has "$diag" 'found under ou=groups,dc=example,dc=org: []' "the test of ${u%:*}"
done

echo "== groupOfUniqueNames, a DN made of the name"
use "$(staticcfg groupOfUniqueNames uniqueMember 'uid=${username},ou=people,dc=example,dc=org' '[]')" "unique names"
expect 201 "$(writes -u erin:erin-ldap-pw-9)" "erin, in the group of unique names"
has "$(as erin)" '"cc-uniq"' "erin's roles"
expect 403 "$(writes -u bob:bob-ldap-pw-9)" "bob, not in it"

echo "== a group search that fails: the sign-in works, without those roles"
use "$(staticcfg groupOfNames member '${dn}' '[]' false ou=missing,dc=example,dc=org)" "a base that does not exist"
expect 200 "$(status -u bob:bob-ldap-pw-9 "$base/-/api/me")" "bob, while the search fails"
hasnot "$(as bob)" '"cc-devs"' "bob's roles while the search fails"
expect 403 "$(writes -u bob:bob-ldap-pw-9)" "bob's upload while the search fails"
expect 401 "$(status -u bob:wrong "$base/-/api/me")" "a wrong password while the search fails"
diag="$(diagnose bob bob-ldap-pw-9)"
has "$diag" '"ok":false' "the test"
has "$diag" '"step":"groups","ok":false' "the test"
has "$diag" 'result code 32' "the test"
has "$(docker logs "$name" 2>&1)" 'ldap group search failed' "the log"

echo "== the Nexus API, the same settings"
nexus="$base/service/rest/v1/security/ldap"
expect 204 "$(status "${adm[@]}" -X DELETE "$nexus/ldap")" "the directory deleted"
body="{\"name\":\"corp\",\"protocol\":\"ldap\",\"host\":\"$ldapname\",\"port\":1389,\"searchBase\":\"dc=example,dc=org\",\"authScheme\":\"SIMPLE\",\"authUsername\":\"$ldapadmin\",\"authPassword\":\"$ldappw\",\"connectionTimeoutSeconds\":5,\"connectionRetryDelaySeconds\":300,\"maxIncidentsCount\":3,\"userBaseDn\":\"ou=people\",\"userSubtree\":true,\"userObjectClass\":\"inetOrgPerson\",\"userIdAttribute\":\"uid\",\"userRealNameAttribute\":\"cn\",\"userEmailAddressAttribute\":\"mail\",\"ldapGroupsAsRoles\":true,\"groupType\":\"static\",\"groupBaseDn\":\"ou=groups\",\"groupSubtree\":false,\"groupObjectClass\":\"posixGroup\",\"groupIdAttribute\":\"cn\",\"groupMemberAttribute\":\"memberUid\",\"groupMemberFormat\":\"\${username}\"}"
expect 201 "$(status "${adm[@]}" -H 'Content-Type: application/json' -X POST -d "$body" "$nexus")" "the Nexus body"
got="$(curl -s "${adm[@]}" "$nexus/corp")"
for field in '"groupType":"static"' '"groupBaseDn":"ou=groups"' '"groupObjectClass":"posixGroup"' '"groupIdAttribute":"cn"' '"groupMemberAttribute":"memberUid"' '"groupMemberFormat":"${username}"' '"ldapGroupsAsRoles":true'; do
  has "$got" "$field" "the Nexus body read back"
done
nosecret "$got" "the Nexus body read back"
got="$(curl -s "${adm[@]}" "$base/-/api/realms/ldap")"
has "$got" '"groupBaseDN":"ou=groups,dc=example,dc=org"' "the same settings, the native form"
has "$got" '"groupMemberAttr":"memberUid"' "the same settings, the native form"
expect 200 "$(status "${adm[@]}" "${ui[@]}" -X PUT -d '{"realms":[{"id":"local","enabled":true},{"id":"token","enabled":true},{"id":"ldap","enabled":true}]}' "$base/-/api/realms")" "the realm on"
expect 201 "$(writes -u carol:carol-ldap-pw-9)" "carol, through the Nexus body"
expect 403 "$(writes -u bob:bob-ldap-pw-9)" "bob, through the Nexus body"

echo "== taken out of the group: gone at the next re-check, tokens too"
use "$(staticcfg groupOfNames member '${dn}' '[]')" "groupOfNames again"
bobjar="$work/bob-cookies.txt"
expect 200 "$(status -c "$bobjar" "${ui[@]}" -X POST -d '{"user":"bob","password":"bob-ldap-pw-9"}' "$base/-/api/login")" "bob signs in"
token="$(curl -s -b "$bobjar" "${ui[@]}" -X POST -d '{}' "$base/-/api/tokens" | sed -nE 's/.*"token":"([^"]+)".*/\1/p')"
[ -n "$token" ] || fail "bob's token"
expect 201 "$(writes -H "Authorization: Bearer $token")" "bob's token, in the group"
docker exec -i "$ldapname" ldapmodify -x -H ldap://localhost:1389 -D "$ldapadmin" -w "$ldappw" >/dev/null <<'EOF2'
dn: cn=cc-devs,ou=groups,dc=example,dc=org
changetype: modify
delete: member
member: uid=bob,ou=people,dc=example,dc=org
EOF2
sleep 61
expect 403 "$(writes -H "Authorization: Bearer $token")" "bob's token, taken out of the group"
expect 200 "$(status -H "Authorization: Bearer $token" "$base/-/api/me")" "bob's token still signs in"

echo "== logs carry no secret"
logs="$(docker logs "$name" 2>&1)"
nosecret "$logs" "server log"

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "LDAP SMOKE OK"
