# Command Center 운영 가이드

> 첫 화면과 소개는 [README](../README.md)에 있습니다. 이 문서는 설치, 프로덕션 배포, 설정, 포맷별 클라이언트 설정, 운영을 자세히 다룹니다.

## 소개

Command Center는 저메모리 패키지 저장소입니다. 에디션은 하나뿐인 오픈소스입니다. Go 단일 바이너리이며 Kubernetes(Helm 차트)나 Docker로 실행하고, 설정 파일은 `config.yaml` 하나입니다.
업계에서 널리 쓰이는 `/repository/<저장소이름>/...` 경로 규칙을 따르므로, 이미 이 규칙으로 설정한 클라이언트는 호스트만 바꾸면 됩니다.

지원 포맷과 저장소 유형:

| 포맷 | hosted (업로드) | proxy (원격 캐시) | group (묶음) |
|---|---|---|---|
| Maven | O | O | O |
| npm | O | O | O |
| PyPI | O | O | O |
| Go | O | O | O |
| Cargo | O | O | O |

- **hosted**: 직접 업로드한 패키지를 저장합니다.
- **proxy**: 원격 저장소(Maven Central, npmjs, pypi.org, proxy.golang.org, crates.io 등)를 처음 요청할 때 가져와 캐시합니다.
- **group**: 여러 저장소를 하나의 URL로 묶어 읽습니다. `writeMember`를 지정하면 업로드를 그 hosted 멤버로 보냅니다.

메모리 사용량(`test/e2e.sh`로 측정):

- 포맷별 스모크 테스트(`docker stats`): Maven 22 MiB, npm 8.4 MiB, PyPI 21 MiB, Go 11 MiB, Cargo 9.9 MiB, UI 7.1 MiB
- 부하 테스트(`test/load/load.sh`): 128 MiB 컨테이너에서 100 MiB 파일 50개 동시 다운로드, 5개 동시 업로드, 적대적 업로드를 처리하는 동안 최대 RSS 53 MiB 이하

## 빠른 시작

Kubernetes에는 Helm 차트로 설치합니다("Kubernetes" 참고). 여기서는 Docker compose로 한 대에 띄우며, compose 파일은 릴리스 이미지 `ghcr.io/sungjinseo/commandcenter`를 씁니다. 아래의 `1.2.3`과 `v1.2.3`은 쓸 [릴리스](https://github.com/sungjinseo/commandcenter/releases)의 버전으로 바꾸세요.

1. 쓸 버전의 태그로 Git 저장소를 받습니다. `docker-compose.yml`과 `config.example.yaml`도 그 버전의 것이어야 합니다(설정 파일에 모르는 키가 있으면 서버가 시작하지 않습니다).

   ```bash
   git clone --depth 1 --branch v1.2.3 https://github.com/sungjinseo/commandcenter.git
   cd commandcenter
   ```

2. `docker-compose.yml`과 같은 폴더에 `.env` 파일을 만들고 쓸 버전을 적습니다. 이미지 태그에는 `v`가 없습니다. compose는 모든 명령에서 이 파일을 읽고, 버전이 없으면 시작하지 않습니다.

   ```
   COMMANDCENTER_VERSION=1.2.3
   ```

   **소스에서 빌드**하려면 이 checkout을 빌드하고 `.env`에 그 이미지를 적습니다. 빌드 인자 `VERSION`이 `commandcenter version`과 UI에 나오는 버전이 됩니다(지정하지 않으면 `dev`).

   ```bash
   docker build --build-arg VERSION=dev -t commandcenter:dev .
   ```

   ```
   COMMANDCENTER_IMAGE=commandcenter
   COMMANDCENTER_VERSION=dev
   ```

3. 사용자 비밀번호 해시를 만듭니다. 명령을 실행한 뒤 비밀번호(8자 이상, 72바이트 이하)를 입력하고 Enter를 누르세요. 직접 빌드했다면 이미지 이름은 `commandcenter:dev`입니다.

   ```bash
   docker run --rm -i ghcr.io/sungjinseo/commandcenter:1.2.3 hash-password
   ```

4. `config.example.yaml`을 `config.yaml`로 복사하고 `passwordHash` 자리에 3번의 출력값을 넣습니다.

   ```bash
   # Git Bash / macOS / Linux
   cp config.example.yaml config.yaml
   ```

   ```powershell
   # PowerShell
   Copy-Item config.example.yaml config.yaml
   ```

   > `docker compose up` 전에 `config.yaml`이 반드시 존재해야 합니다. 없으면 Docker가 같은 이름의 디렉터리를 만들어 서버 시작에 실패합니다.

5. 실행합니다.

   ```bash
   docker compose up -d
   ```

   상태 확인: `http://localhost:8081/service/rest/v1/status` 가 200을 반환하면 정상입니다.

## Kubernetes

Kubernetes에는 Helm 차트로 설치합니다. 차트는 서버 Pod 하나(StatefulSet), `/data`의 ReadWriteOnce 볼륨, `config.yaml`을 담은 Secret, Service, 그리고 켜면 Ingress를 만듭니다. 차트는 릴리스마다 `oci://ghcr.io/sungjinseo/charts/commandcenter`에 올라갑니다. 차트 버전은 릴리스 버전(`v1.2.3`이면 `1.2.3`)과 같고, 기본 이미지도 같은 버전의 `ghcr.io/sungjinseo/commandcenter`입니다. OCI 레지스트리의 차트를 받으려면 Helm 3.8 이상이 필요합니다. 차트의 원본은 [`deploy/helm/commandcenter`](../deploy/helm/commandcenter)이고, Helm 없이 설치하려면 "참조 매니페스트"를 보세요.

이 절의 명령은 네임스페이스와 릴리스 이름으로 모두 `commandcenter`를 씁니다. 그러면 StatefulSet과 Service는 `commandcenter`, Pod는 `commandcenter-0`, PVC는 `data-commandcenter-0`, 설정 Secret은 `commandcenter-config`입니다. 릴리스 이름에 `commandcenter`가 들어 있지 않으면 이름이 `<릴리스 이름>-commandcenter`가 됩니다.

### Helm 차트로 설치

1. 관리자 비밀번호의 해시를 만듭니다. 명령을 실행한 뒤 비밀번호(8자 이상, 72바이트 이하)를 입력하고 Enter를 누르세요. Docker가 없으면 GitHub Release의 linux 바이너리로 `commandcenter hash-password`를 실행해도 됩니다.

   ```bash
   docker run --rm -i ghcr.io/sungjinseo/commandcenter:1.2.3 hash-password
   ```

2. 네임스페이스와 비밀값 키 Secret을 만듭니다. 서버는 이 키로 SMTP 비밀번호, LDAP bind 비밀번호, proxy `remoteAuth`를 암호화해 저장합니다("저장된 비밀값 암호화" 참고). 마지막 명령으로 키 값을 꺼내 볼륨 백업과 **다른 곳**에 보관하세요. 키를 잃으면 서버가 시작하지 않고 저장된 비밀값은 되살릴 수 없습니다. 이 Secret은 Helm이 만든 것이 아니라서 `helm uninstall`로는 지워지지 않지만, 네임스페이스를 지우면 함께 지워집니다.

   ```bash
   kubectl create namespace commandcenter
   kubectl -n commandcenter create secret generic commandcenter-secret-key \
     --from-literal=key="$(head -c 32 /dev/urandom | base64)"
   kubectl -n commandcenter get secret commandcenter-secret-key -o jsonpath='{.data.key}' | base64 -d
   ```

3. 값 파일(예: `commandcenter-values.yaml`)을 만듭니다. 앞의 세 값은 반드시 정합니다("차트 값" 참고). 관리자 비밀번호의 해시가 들어 있으니 비밀번호처럼 다루고, 업그레이드할 때도 이 파일을 다시 씁니다.

   ```yaml
   config:
     baseURL: https://repo.example.com          # 클라이언트가 접속하는 주소
     adminPasswordHash: '<1번의 출력>'            # 작은따옴표로 감쌉니다
   persistence:
     storageClass: <블록 스토리지 StorageClass>    # NFS·SMB는 안 됩니다
   secretKey:
     existingSecret: commandcenter-secret-key
   ingress:
     enabled: true
     className: nginx
     annotations:                               # "Ingress와 큰 업로드" 참고
       nginx.ingress.kubernetes.io/proxy-body-size: "0"
       nginx.ingress.kubernetes.io/proxy-request-buffering: "off"
       nginx.ingress.kubernetes.io/proxy-read-timeout: "300"
       nginx.ingress.kubernetes.io/proxy-send-timeout: "300"
     hosts:
       - host: repo.example.com
         paths: [/]
     tls:
       - secretName: repo-example-com-tls       # 인증서 Secret(cert-manager 등으로 만듭니다)
         hosts: [repo.example.com]
   ```

4. 설치하고 테스트합니다. `--wait`는 Pod가 준비될 때까지 기다립니다.

   ```bash
   helm install commandcenter oci://ghcr.io/sungjinseo/charts/commandcenter --version 1.2.3 \
     -n commandcenter -f commandcenter-values.yaml --wait
   helm test commandcenter -n commandcenter
   ```

   `https://repo.example.com/`에서 `admin`과 1번의 비밀번호로 로그인합니다. 클라이언트도 이 주소를 씁니다.

### 차트 값

모든 값은 차트의 `values.yaml`에 주석과 함께 있습니다(`helm show values oci://ghcr.io/sungjinseo/charts/commandcenter --version 1.2.3`).

- **`config.baseURL`(필수)**: 클라이언트가 접속하는 주소입니다. TLS를 Ingress에서 끝내면 그 `https://` 주소입니다("TLS와 리버스 프록시" 참고).
- **`config.adminPasswordHash`(필수)**: `admin` 사용자의 bcrypt 해시(`hash-password`의 출력)입니다. 필수 값이 비어 있으면 차트가 설치를 거부합니다.
- **`persistence.storageClass`(반드시 정하기)**: `/data` 볼륨의 StorageClass입니다. **블록 스토리지만 됩니다.** `meta.db`는 WAL 모드의 SQLite라서 NFS·SMB 같은 네트워크 파일시스템에서는 깨질 수 있습니다("저장 공간과 인스턴스" 참고). 비워 두면 클러스터의 기본 StorageClass를 쓰는데 그것이 NFS인 클러스터도 있으니 이름을 직접 적으세요. `kubectl get storageclass`의 PROVISIONER를 보고 고릅니다.
  - 됩니다: 클라우드 블록 디스크(AWS EBS `ebs.csi.aws.com`, GCE PD `pd.csi.storage.gke.io`, Azure Disk `disk.csi.azure.com`, NKS의 `nks-block-storage`), Ceph RBD, Longhorn, iSCSI, 로컬 볼륨
  - 안 됩니다: NFS(`nfs.csi.k8s.io`, nfs-subdir-external-provisioner), NAS 상품, AWS EFS, Azure Files, GKE Filestore, CephFS
  - `-`를 적으면 `storageClassName: ""`이 되어 미리 만들어 둔 PV에 묶입니다.
- **첫 시작에만 쓰는 값**: `config`의 `baseURL`, `adminPasswordHash`, `anonymousRead`, `repositories`, 그리고 `config.extra`에 넣은 `users`, `roles`, `maxUploadMiB`는 처음 시작할 때 한 번만 DB로 가져옵니다("설정 레퍼런스" 참고). 설치한 뒤에는 웹 UI에서 바꾸세요. 값 파일에서 바꿔 `helm upgrade`를 해도 Pod만 다시 시작되고 반영되지 않습니다.
- `secretKey.existingSecret`, `secretKey.key`(기본 `key`): 비밀값 키가 든 Secret의 이름과 그 안의 키 이름입니다. 서버에는 환경변수 `COMMANDCENTER_SECRET_KEY`로 들어갑니다. 비워 두면 비밀값을 평문으로 저장합니다.
- `ingress.enabled`, `ingress.className`, `ingress.annotations`, `ingress.hosts`, `ingress.tls`: "Ingress와 큰 업로드" 참고.
- `persistence.size`(기본 `50Gi`): blob과 `meta.db`가 들어갑니다. proxy 캐시는 Cleanup 정책이 지울 때까지 계속 커집니다. `persistence`의 값은 설치한 뒤 `helm upgrade`로 바꿀 수 없습니다("차트 업그레이드" 참고).
- `config.anonymousRead`(기본 `false`), `config.repositories`(첫 시작에 만들 저장소, `config.example.yaml`의 형식), `config.extra`(그 밖의 `config.yaml` 최상위 키. `users` 목록을 넣으면 `admin` 사용자 대신 그 목록을 씁니다), `config.existingSecret`(직접 만든 Secret의 `config.yaml` 키를 그대로 씁니다. 이때 다른 `config` 값은 비워 둡니다).
- `image.repository`, `image.tag`(기본은 차트의 appVersion), `imagePullSecrets`: 사내 레지스트리로 옮긴 이미지나 직접 빌드한 이미지를 쓸 때 바꿉니다. 태그는 버전으로 고정하고 `latest`는 쓰지 마세요.
- `resources`(기본: 메모리 요청과 한도 `128Mi`, CPU 요청 `100m`, CPU 한도 없음), `goMemLimit`(기본 `80MiB`): 메모리 한도를 올리면 `goMemLimit`도 한도의 60~65 % 정도로 함께 올립니다("리소스", "메모리 설정" 참고).
- `proxy.http`, `proxy.https`, `proxy.noProxy`: proxy 저장소가 사내 프록시를 거쳐 원격에 접속할 때 씁니다(`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`). 클러스터 안의 원격은 `proxy.noProxy`에 넣으세요(예: `.svc,.cluster.local`).
- `extraEnv`: 예약 작업의 시간대(`TZ`, "예약 작업" 참고) 같은 환경변수입니다. 예: `extraEnv: [{name: TZ, value: Asia/Seoul}]`
- `extraVolumes`, `extraVolumeMounts`: 이름 붙은 blob store를 둘 볼륨입니다. blob은 NFS에 둘 수 있지만 `meta.db`는 안 됩니다("저장 공간과 인스턴스" 참고).
- `terminationGracePeriodSeconds`(기본 60), `startupProbe`, `readinessProbe`, `livenessProbe`, `podSecurityContext`, `securityContext`: 바꿀 일이 거의 없습니다("동작 방식" 참고).
- 그 밖에 `service`, `serviceAccount`, `nodeSelector`, `tolerations`, `affinity`, `priorityClassName`, `podAnnotations`, `podLabels`, `nameOverride`, `fullnameOverride`가 있습니다. 릴리스 이름과 `nameOverride`·`fullnameOverride`는 처음 설치할 때 정합니다. 바꾸면 빈 볼륨을 가진 새 StatefulSet이 생깁니다. Pod 수를 정하는 값은 일부러 두지 않았습니다.

### Ingress와 큰 업로드

서버는 평문 HTTP만 받으므로 TLS는 Ingress(또는 Gateway)에서 끝내고, `config.baseURL`을 그 `https://` 주소와 똑같이 둡니다("TLS와 리버스 프록시" 참고). 컨트롤러의 기본값은 패키지 업로드에 맞지 않습니다. ingress-nginx는 기본으로 요청 본문을 1 MiB까지만 받아 큰 jar, tarball, wheel, crate 업로드가 413으로 끊기고, 서버의 응답을 60초까지만 기다립니다. ingress-nginx라면 `ingress.annotations`에 다음을 넣으세요(위 값 파일의 예).

| 어노테이션 | 값 | 뜻 |
|---|---|---|
| `nginx.ingress.kubernetes.io/proxy-body-size` | `"0"` | 본문 크기를 Ingress에서 보지 않고 서버의 `maxUploadMiB`(기본 1024 MiB, 넘으면 413)에 맡깁니다. Ingress에서도 막으려면 `maxUploadMiB` 이상(예: `"1024m"`)으로 둡니다. |
| `nginx.ingress.kubernetes.io/proxy-request-buffering` | `"off"` | 업로드를 컨트롤러에 다 받아 두지 않고 바로 서버로 넘깁니다. 켜 두면 업로드마다 그 크기만큼의 임시 디스크가 컨트롤러 Pod에 필요하므로, 크기 제한을 `"0"`으로 둘 때는 꼭 끕니다. |
| `nginx.ingress.kubernetes.io/proxy-read-timeout` | `"300"` | 서버의 응답을 기다리는 시간(초). proxy 저장소가 원격에서 처음 받아 오는 요청은 응답이 늦을 수 있습니다. |
| `nginx.ingress.kubernetes.io/proxy-send-timeout` | `"300"` | 서버가 요청 본문을 받아 가기를 기다리는 시간(초). |

- 본문 수신 타임아웃과 클라이언트 IP당 연결 수("TLS와 리버스 프록시"의 "프록시에서 제한할 것")는 ingress-nginx ConfigMap의 `client-body-timeout`(기본 60초)과 어노테이션 `nginx.ingress.kubernetes.io/limit-connections`로 겁니다. 연결 수 제한은 컨트롤러가 실제 클라이언트 주소를 볼 때만 쓰세요. 로드밸런서의 주소로 보이면 모든 클라이언트가 한 주소로 묶여 제한됩니다.
- Gateway API(HTTPRoute)를 쓰면 `ingress.enabled: false`로 두고 `httpRoute.enabled: true`, `httpRoute.parentRefs`(붙을 Gateway, 예: `[{name: traefik-gateway, namespace: traefik}]`), `httpRoute.hostnames`를 정합니다. 차트가 Service의 8081 포트로 가는 HTTPRoute를 만듭니다. 본문 크기와 타임아웃은 Gateway 구현에서 정합니다. 예를 들어 Traefik v3는 요청 전체를 읽는 시간(entryPoint의 `transport.respondingTimeouts.readTimeout`)이 기본 60초라서 그보다 오래 걸리는 업로드가 끊기므로 늘려야 합니다.

### 설치 확인(helm test)

```bash
helm test commandcenter -n commandcenter
```

테스트 Pod(`commandcenter-test-status`)가 서버 이미지의 `commandcenter healthcheck -url`로 Service를 거쳐 `GET /service/rest/v1/status`를 부르고, 200이면 성공합니다. 서버 이미지만 쓰므로 폐쇄망에서도 다른 이미지가 필요 없습니다. 성공한 테스트 Pod는 지워지고 실패한 Pod는 남으므로 `kubectl -n commandcenter logs commandcenter-test-status`로 원인을 봅니다. HTTP 서버가 Service 뒤에서 응답하는지만 보며, DB와 디스크는 관리자 화면의 시스템 상태에서 확인합니다.

### 차트 업그레이드

1. 올릴 버전의 [CHANGELOG](../CHANGELOG.md)를 읽습니다.
2. 백업합니다("Kubernetes에서 백업" 참고). 업그레이드한 뒤 되돌릴 수 있는 방법은 이 백업뿐입니다.
3. 설치 때의 값 파일로 새 차트 버전을 적용하고 테스트합니다.

   ```bash
   helm upgrade commandcenter oci://ghcr.io/sungjinseo/charts/commandcenter --version 1.2.4 \
     -n commandcenter -f commandcenter-values.yaml --wait
   helm test commandcenter -n commandcenter
   ```

- StatefulSet은 이전 Pod가 끝난 뒤에(최대 60초, "정상 종료" 참고) 새 Pod를 띄우므로 그사이 서비스가 잠시 멈춥니다. 새 버전은 처음 시작할 때 `meta.db`를 새 구조로 바꾸며, startupProbe가 5분까지 기다립니다.
- 이미지는 차트 버전을 따라갑니다. `image.tag`를 정해 두었다면 함께 바꾸세요.
- `persistence`의 값(`storageClass`, `size`, `accessModes`)은 바꾸지 않습니다. StatefulSet의 볼륨 템플릿은 만든 뒤 바꿀 수 없어서 `helm upgrade`가 `Forbidden: updates to statefulset spec for fields other than …` 오류로 실패합니다. 볼륨을 늘리려면 StorageClass가 `allowVolumeExpansion: true`일 때 PVC를 직접 늘리고, 값 파일의 `size`는 그대로 둡니다.

  ```bash
  kubectl -n commandcenter patch pvc data-commandcenter-0 -p '{"spec":{"resources":{"requests":{"storage":"100Gi"}}}}'
  ```

- **버전을 올린 업그레이드를 `helm rollback`으로 되돌리지 마세요.** 다운그레이드는 지원하지 않습니다("업그레이드" 참고). 되돌려야 하면 "Kubernetes에서 백업"의 복원으로 업그레이드 전의 스냅샷을 되살리되, 마지막 `--replicas=1` 대신 이전 차트 버전으로 `helm upgrade`를 실행합니다. 차트가 Pod를 이전 이미지로 다시 띄웁니다. 값만 바꾼 리비전은 `helm rollback`해도 됩니다.

### Kubernetes에서 백업

`meta.db`와 blob은 같은 시점의 것이어야 하므로("백업과 복원" 참고) Pod를 멈추고 받습니다. 클러스터가 CSI 볼륨 스냅샷을 지원하면(`kubectl get volumesnapshotclass`) 스냅샷이 가장 빠르며, 서비스는 스냅샷이 준비될 때까지 멈춥니다.

```bash
NS=commandcenter SNAP=commandcenter-$(date +%Y%m%d-%H%M)
kubectl -n $NS scale statefulset commandcenter --replicas=0
kubectl -n $NS wait --for=delete pod/commandcenter-0 --timeout=120s
kubectl -n $NS apply -f - <<EOF
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: $SNAP
spec:
  volumeSnapshotClassName: <VolumeSnapshotClass>   # 기본 클래스를 쓰면 이 줄을 지웁니다
  source:
    persistentVolumeClaimName: data-commandcenter-0
EOF
kubectl -n $NS wait volumesnapshot/$SNAP --for=jsonpath='{.status.readyToUse}'=true --timeout=30m
kubectl -n $NS scale statefulset commandcenter --replicas=1
```

- 백업하는 동안에는 `helm upgrade`를 하지 마세요(GitOps 도구의 자동 동기화도 멈춥니다). 차트가 Pod를 다시 띄웁니다.
- `extraVolumes`에 둔 blob store가 있으면 같은 정지 시간에 그 볼륨도 찍습니다.
- VolumeSnapshot은 네임스페이스에 속합니다. 네임스페이스를 지우면 함께 지워지고, VolumeSnapshotClass의 `deletionPolicy`가 `Delete`이면 스토리지의 스냅샷도 사라집니다. 오래 둘 백업은 `Retain` 클래스로 찍거나 클러스터 밖에도 두세요.
- 비밀값 키 Secret과 값 파일은 스냅샷에 들어가지 않습니다. 따로 보관하되 키는 스냅샷과 다른 곳에 둡니다.

**복원**: Pod를 멈추고 PVC를 스냅샷에서 다시 만듭니다. StatefulSet은 이름이 같은 PVC를 그대로 씁니다. 지금 PVC의 데이터는 지워지므로 필요하면 먼저 지금 상태도 스냅샷으로 남기세요.

```bash
kubectl -n $NS scale statefulset commandcenter --replicas=0
kubectl -n $NS wait --for=delete pod/commandcenter-0 --timeout=120s
kubectl -n $NS delete pvc data-commandcenter-0
kubectl -n $NS apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data-commandcenter-0
spec:
  storageClassName: <persistence.storageClass와 같은 값>
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 50Gi            # 스냅샷의 RESTORESIZE(kubectl get volumesnapshot) 이상
  dataSource:
    apiGroup: snapshot.storage.k8s.io
    kind: VolumeSnapshot
    name: $SNAP
EOF
kubectl -n $NS scale statefulset commandcenter --replicas=1
```

**스냅샷을 쓸 수 없을 때**: "백업과 복원"의 멈추고 복사 방법을 "서버를 멈춰야 하는 작업"의 일회용 Pod에서 실행합니다. 복원은 볼륨을 비운 뒤에 풉니다(오래된 `meta.db-wal`·`-shm`이 섞이지 않게).

```bash
kubectl -n $NS exec commandcenter-maint -- tar czf - -C / data > data.tgz                                            # 백업
kubectl -n $NS exec -i commandcenter-maint -- sh -c 'find /data -mindepth 1 -delete && tar xzf - -C /' < data.tgz   # 복원
```

### 서버를 멈춰야 하는 작업

멈춘 상태의 백업·복원, 비밀값 키 교체, 키를 잃었을 때의 정리("저장된 비밀값 암호화" 참고)는 서버 Pod를 내리고, 같은 PVC를 마운트해 서버와 같은 UID 65532로 도는 일회용 Pod에서 합니다. 다른 UID로 돌리면 그 사용자 소유의 파일이 볼륨에 남아 서버가 열지 못할 수 있습니다.

```bash
kubectl -n commandcenter scale statefulset commandcenter --replicas=0
kubectl -n commandcenter wait --for=delete pod/commandcenter-0 --timeout=120s
kubectl -n commandcenter apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: commandcenter-maint
spec:
  restartPolicy: Never
  securityContext: {runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532, fsGroup: 65532, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: maint
      image: busybox:1.37
      command: ["sleep", "86400"]
      securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
      volumeMounts: [{name: data, mountPath: /data}]
  volumes:
    - name: data
      persistentVolumeClaim: {claimName: data-commandcenter-0}
EOF
kubectl -n commandcenter wait --for=condition=Ready pod/commandcenter-maint
# 작업: kubectl -n commandcenter exec -it commandcenter-maint -- sh
kubectl -n commandcenter delete pod commandcenter-maint
kubectl -n commandcenter scale statefulset commandcenter --replicas=1
```

서버의 명령(`secret-key rotate` 등)은 busybox 대신 지금 버전의 서버 이미지로 돌립니다. `command` 대신 `args`에 명령을 주고, 설정 Secret `commandcenter-config`를 `/etc/commandcenter`에 마운트하며, 지금 키를 `COMMANDCENTER_SECRET_KEY`(Secret 참조)로 줍니다. 명령이 끝나면 Pod가 멈추므로 출력은 `kubectl -n commandcenter logs commandcenter-maint`로 봅니다.

### 삭제(helm uninstall)

```bash
helm uninstall commandcenter -n commandcenter
```

StatefulSet, Service, Ingress, 설정 Secret은 지워지지만 **PVC `data-commandcenter-0`와 데이터는 남습니다.** StatefulSet이 만든 PVC는 Helm 릴리스에 속하지 않기 때문입니다. 같은 네임스페이스에 같은 릴리스 이름으로 다시 설치하면 이 PVC를 그대로 쓰고, `config`의 첫 시작 값은 다시 가져오지 않습니다. 직접 만든 비밀값 키 Secret도 남습니다(스냅샷을 보관한다면 키도 보관하세요). 데이터까지 지우려면 PVC를 지웁니다. 되돌릴 수 없고, StorageClass의 `reclaimPolicy`가 `Delete`이면 디스크도 지워집니다.

```bash
kubectl -n commandcenter delete pvc data-commandcenter-0
```

### 동작 방식

- **StatefulSet, Pod 하나**: StatefulSet은 이전 Pod가 완전히 끝난 뒤에 새 Pod를 띄우므로 업데이트 중에도 두 프로세스가 같은 `meta.db`를 열지 않습니다(Deployment의 롤링 업데이트는 겹칠 수 있습니다). `replicas`를 늘리면 Pod마다 빈 볼륨이 따로 생겨, 데이터가 서로 다른 서버가 여러 대 생깁니다. 차트에 Pod 수를 정하는 값이 없는 것도 이 때문입니다.
- **볼륨**: `/data`는 ReadWriteOnce PVC(`data-commandcenter-0`, 기본 50Gi)이며 블록 스토리지여야 합니다("차트 값"의 `persistence.storageClass`).
- **노드 장애**: Pod가 `Terminating`이나 `Unknown`에 머물러도 `kubectl delete pod --force --grace-period=0`으로 지우지 마세요. 이전 노드에서 프로세스가 아직 돌고 있으면 같은 볼륨을 두 프로세스가 쓰게 될 수 있습니다.
- **프로브**: startupProbe와 readinessProbe는 `GET /service/rest/v1/status`(인증 없이 200)입니다. HTTP 서버가 응답하는지만 보고 DB나 디스크는 보지 않습니다(관리자 화면의 시스템 상태에서 확인합니다). 처음 시작하거나 업그레이드로 DB를 바꾸는 데 시간이 걸려도 재시작되지 않도록 startupProbe가 5분까지 기다립니다. livenessProbe는 포트에 TCP 연결만 해 봅니다(`tcpSocket`). TCP 연결은 서버의 연결 자리 1024개가 모두 차 있어도 커널이 받아 주므로(커널 대기열이 넘치지 않는 한), 서버가 바쁘거나 공격을 받아 느려졌다고 Pod가 재시작되어 진행 중인 전송이 모두 끊기지는 않습니다. 그동안 readinessProbe가 실패하면 Pod는 Service에서 잠시 빠졌다가 다시 들어옵니다. `commandcenter healthcheck`를 exec 프로브로 쓰지 않는 것은 128 MiB 한도 안에서 프로세스를 하나 더 띄우지 않기 위해서입니다.
- **보안**: UID 65532(non-root), 읽기 전용 루트 파일시스템, 모든 capability 제거, seccomp `RuntimeDefault`로 Pod Security Standards의 `restricted` 기준에 맞습니다. SQLite 임시 파일은 `/tmp`(emptyDir)에 씁니다. 이름 붙은 blob store는 경로를 비우면 `/data/blobstores/<이름>`에 만들어지고, `/data` 밖의 절대 경로에 두려면 그 경로에 볼륨을 따로 마운트해야 합니다(`extraVolumes`, `extraVolumeMounts`). 루트 파일시스템이 읽기 전용이라 다른 경로에는 만들 수 없고, `/tmp`는 Pod가 다시 만들어지면 비워집니다.
- **리소스**: 메모리 요청과 한도 128Mi, `GOMEMLIMIT=80MiB`(`goMemLimit`)입니다. CPU는 요청(100m)만 두고 한도는 두지 않습니다.
- **운영 명령**: "운영"의 `docker compose exec commandcenter …`는 `kubectl -n commandcenter exec commandcenter-0 -- …`로 바꿔 씁니다(예: `kubectl -n commandcenter exec commandcenter-0 -- commandcenter token create -user ci`). 로그는 `kubectl -n commandcenter logs commandcenter-0`입니다.
- **비상 복구**: `user reset-password`는 실행 중인 Pod에서 돌립니다(`printf '%s\n' "$PW" | kubectl exec -i -n commandcenter commandcenter-0 -- commandcenter user reset-password -user admin -admin`). 서버가 5초 안에 반영합니다("비상 복구" 참고).

### 참조 매니페스트

Helm 없이 설치하려면 [`deploy/kubernetes/commandcenter.yaml`](../deploy/kubernetes/commandcenter.yaml)을 씁니다. ConfigMap(`config.yaml`), Service, StatefulSet으로 되어 있고 Pod 구성(볼륨, 프로브, 보안, 리소스, 중지 대기 시간)은 차트와 같습니다. 파일 맨 위 주석대로 이미지, `config.yaml`의 `baseURL`과 관리자 `passwordHash`, `storageClassName`을 채운 뒤 적용합니다. 이미지는 버전을 고정한 릴리스 이미지(`ghcr.io/sungjinseo/commandcenter:1.2.3`, "릴리스 이미지와 바이너리" 참고)나 직접 빌드해 올린 이미지입니다.

```bash
kubectl apply -n <네임스페이스> -f deploy/kubernetes/commandcenter.yaml
```

이름(`commandcenter`, `commandcenter-0`, `data-commandcenter-0`)이 차트와 같으므로 위의 백업, 서버를 멈춰야 하는 작업, 동작 방식이 그대로 적용됩니다. 업그레이드는 이미지 태그를 바꿔 다시 적용하고, Ingress와 비밀값 키(주석 처리된 `COMMANDCENTER_SECRET_KEY` 항목)는 직접 넣습니다.

## 프로덕션 배포

빠른 시작은 한 대에서 시험해 보는 구성입니다. 팀이 매일 쓰는 저장소로 올리기 전에 이 절을 확인하세요.

### 체크리스트

1. 검증한 버전을 고정해서 씁니다(`latest`나 기본 브랜치를 그대로 따라가지 않기). 업그레이드는 백업한 뒤에 하고, 다운그레이드는 지원하지 않습니다("업그레이드" 참고).
2. TLS는 리버스 프록시에서 끝내고, 일반 설정의 `baseURL`을 `https://` 주소로 둡니다. 서버 포트 8081은 프록시에서만 닿게 합니다.
3. 사내 전용 저장소라면 익명 읽기를 끕니다. 일반 설정의 `anonymousRead`와 저장소별 `anonymousRead`를 확인하세요. `config.example.yaml`은 시험하기 편하도록 `anonymousRead: true`입니다. 권한이 없는 로그인 사용자의 읽기까지 막으려면 `authenticatedRead`도 끕니다("읽기 권한" 참고).
4. 프록시에서 요청 본문 크기, 본문 수신·응답 전송 타임아웃, 클라이언트당 연결 수를 제한합니다.
5. `meta.db`가 있는 `dataDir`는 로컬 디스크나 블록 스토리지에 둡니다. NFS·SMB에는 두지 않습니다. 인스턴스는 하나만 띄웁니다.
6. 메모리 한도와 `GOMEMLIMIT`을 함께 정합니다.
7. 중지 대기 시간을 60초로 둡니다(compose 파일, Helm 차트, 참조 매니페스트에는 들어 있습니다. "정상 종료" 참고).
8. 정기 백업을 돌리고, 복원도 한 번 해 봅니다("백업과 복원" 참고).
9. 저장된 비밀값을 암호화할 키(`COMMANDCENTER_SECRET_KEY` 또는 `secretKeyFile`)를 정하고, 키는 `meta.db` 백업과 따로 보관합니다("저장된 비밀값 암호화" 참고).

### TLS와 리버스 프록시

서버는 평문 HTTP만 받습니다. 비밀번호와 토큰이 오가므로 사내망이라도 앞에 리버스 프록시(nginx, HAProxy, Caddy, Traefik, Kubernetes Ingress·Gateway 등)를 두고 거기서 TLS를 끝내세요.

- **`baseURL`은 `https://` 주소**: 일반 설정의 `baseURL`을 클라이언트가 실제로 접속하는 주소와 똑같이 맞춥니다. npm·Cargo 응답의 다운로드 URL, PyPI 파일 링크의 경로 앞부분, 세션 쿠키의 이름·`Secure` 플래그(https면 `__Host-cc_session`)가 이 값으로 정해집니다. 서버는 `X-Forwarded-Proto`, `X-Forwarded-Host` 같은 헤더를 보지 않습니다.
- **브라우저의 교차 출처 쓰기 차단**: 다른 사이트의 페이지가 보낸 쓰기 요청(업로드, 삭제, API 변경)은 403으로 거부합니다. 브라우저가 기억한 Basic 인증 정보가 그런 요청에 실려 가도 업로드되지 않게 하기 위해서입니다. `Sec-Fetch-Site`나 `Origin` 헤더를 보내지 않는 명령줄 클라이언트는 영향이 없습니다. 웹 UI는 HTTPS라면 브라우저가 `Sec-Fetch-Site: same-origin`을 보내므로 늘 통과합니다. 평문 HTTP에서는 `Origin`을 `Host`와 비교하는데, 프록시가 `Host`를 바꾸면(nginx 기본값) 둘이 어긋나므로 `baseURL`의 출처(스킴, 호스트, 포트)와 같은 `Origin`도 허용합니다. `baseURL`이 실제 주소와 다르면 이때 웹 UI의 업로드가 403이 됩니다.
- **8081은 프록시만**: compose라면 `ports: ["127.0.0.1:8081:8081"]`처럼 프록시가 있는 호스트에만 열거나, 프록시와 같은 Docker 네트워크에서만 노출하세요. 그러지 않으면 프록시의 TLS와 제한을 건너뛰는 평문 경로가 남습니다.
- **프록시에서 제한할 것**: 서버도 동시 연결을 1024개로 제한하고, 자리가 차면 요청 사이에 쉬고 있는 연결이나 1초 넘게 헤더를 다 보내지 않은 연결부터 닫아 새 연결을 받으며, 멈춘 본문을 끊습니다("메모리 설정", "업로드" 참고). 그러나 요청을 처리 중인 연결은 닫지 않습니다. 응답을 받아 가지 않고 붙들고 있는 연결처럼 처리 중인 연결이 자리를 모두 차지하면 다른 클라이언트는 기다려야 하므로, 외부에 노출한다면 연결 수와 본문 수신·응답 전송 시간 제한은 앞단에서 거세요.
  - 요청 본문 크기: `maxUploadMiB`(기본 1024 MiB) 이상으로 둡니다. 작으면 큰 업로드가 프록시에서 413으로 끊깁니다.
  - 본문 수신 타임아웃: 본문이 일정 시간(예: 60초) 동안 한 바이트도 오지 않으면 끊습니다. 느려도 계속 오는 업로드는 끊기지 않습니다.
  - 응답 전송 타임아웃: 클라이언트가 일정 시간(예: 60초) 동안 응답을 받아 가지 않으면 끊습니다. 서버 자체에는 이 제한이 없습니다(nginx는 `send_timeout`, 기본 60초).
  - 클라이언트 IP당 동시 연결 수: 빌드 서버 여러 대가 NAT 하나 뒤에 있으면 넉넉히 잡으세요.
  - 응답 대기 시간: proxy 저장소가 원격에서 처음 받아 오는 요청은 응답이 늦을 수 있으므로 넉넉히(예: 300초) 둡니다.
- **요청 로그**: 로그의 `remote`는 프록시의 주소가 됩니다(`X-Forwarded-For`는 기록하지 않습니다). 클라이언트 주소는 프록시 로그에서 확인하세요.

nginx 예시:

```nginx
# http { } 블록 안
limit_conn_zone $binary_remote_addr zone=cc_conn:10m;

server {
    listen 443 ssl;
    server_name repo.example.com;
    ssl_certificate     /etc/nginx/tls/repo.example.com.crt;
    ssl_certificate_key /etc/nginx/tls/repo.example.com.key;

    client_max_body_size  1024m;  # maxUploadMiB 이상
    client_header_timeout 10s;
    client_body_timeout   60s;    # 본문이 60초 동안 오지 않으면 끊음
    limit_conn cc_conn 100;       # 클라이언트 IP당 동시 연결

    location / {
        proxy_pass http://127.0.0.1:8081;
        proxy_read_timeout 300s;
        proxy_send_timeout 300s;
    }
}
```

nginx는 기본적으로 요청 본문을 끝까지 받은 다음 서버로 넘깁니다(`proxy_request_buffering on`). 그래서 본문을 멈춘 연결은 서버까지 오지 않습니다. 대신 큰 업로드는 nginx의 임시 디렉터리를 거치므로 그만큼의 디스크가 필요합니다.

### 저장 공간과 인스턴스

- **`meta.db`는 로컬 디스크나 블록 스토리지에**: `dataDir`(기본 `/data`)에는 `meta.db`(SQLite, WAL 모드)와 default blob store(`blobs/`)가 있습니다. WAL은 같은 호스트의 공유 메모리 파일(`meta.db-shm`)과 파일 잠금에 기대므로, NFS나 SMB(CIFS) 같은 네트워크 파일시스템에서는 잠금이 보장되지 않고 DB가 깨질 수 있습니다. 로컬 디스크, 클라우드 블록 디스크, iSCSI, Kubernetes의 ReadWriteOnce 블록 볼륨을 쓰세요.
- **NFS에는 blob만**: blob은 내용 주소 방식이라 한 번 쓰면 바뀌지 않으므로, 이름 붙은 blob store("Blob Store" 참고)는 NFS 경로에 둘 수 있습니다. compact 작업이 파일의 수정 시각으로 진행 중인 업로드를 보호하므로 NFS 서버와 시계를 맞춰 두세요(NTP).
- **blob store 경로는 영구 볼륨에**: 컨테이너 안의 경로(예: `/tmp/…`)에 store를 만들면 컨테이너를 다시 만들 때 blob이 사라지고 DB 기록만 남습니다.
- **인스턴스는 하나만**: 같은 경로 업로드의 잠금, 예약 작업, 세션, 캐시가 모두 프로세스 안에 있습니다. 같은 `dataDir`나 blob store를 두 프로세스가 함께 쓰면 안 되고, 이중화(active-active)나 여러 대로 늘리는 구성은 지원하지 않습니다. 컨테이너를 바꿀 때는 이전 컨테이너가 완전히 멈춘 뒤에 새 컨테이너를 띄우세요. 대신 재시작은 빠릅니다(중지와 기동 모두 보통 1초 안쪽).
- **디스크 여유**: proxy 캐시는 계속 커집니다. Cleanup 정책과 compact 작업으로 정리하고, Blob Store 화면에서 파일시스템의 남은 용량을 지켜보세요.

### 리소스

- **메모리**: 기본 구성은 컨테이너 한도 128 MiB에 `GOMEMLIMIT=80MiB`이며, 이 구성의 측정값은 "소개"의 메모리 사용량에 있습니다. `docker stats`의 값에는 큰 파일을 읽고 쓸 때의 커널 파일 캐시가 들어가 한도에 가까워 보일 수 있지만, 커널이 회수하는 메모리입니다. 한도를 올릴 때는 `GOMEMLIMIT`도 함께 올리되 한도보다 충분히 낮게 두세요("메모리 설정" 참고).
- **CPU**: 대부분 업로드 해시 계산(sha256, sha1, md5, sha512)과 bcrypt 비밀번호 확인에 씁니다. CPU 한도를 낮게 걸면 로그인과 큰 업로드가 느려집니다.
- **디스크**: 같은 blob store 안에서는 같은 내용을 한 번만 저장하므로(hosted와 proxy가 같은 파일을 공유) 필요한 용량은 hosted 패키지와 proxy 캐시를 합친 크기입니다. 디스크가 차면 업로드가 실패합니다.

### 정상 종료

서버는 SIGTERM(또는 SIGINT)을 받으면 새 연결을 받지 않고 진행 중인 요청을 최대 30초 기다리며, 그때까지 끝나지 않은 요청은 끊습니다. 연결 자리 1024개가 모두 처리 중일 때도 30초에 끝나며, 자리를 기다리던 새 연결은 바로 닫습니다. 그 뒤 다운로드 수·요청 통계, 보낼 메일, 활동 기록을 각각 최대 10초 동안 마저 쓰고 DB를 닫습니다. 실행 중인 예약 작업은 취소됩니다.

Docker의 기본 중지 대기 시간은 10초라서, 그 안에 끝나지 않으면 프로세스가 강제로 종료됩니다. 진행 중인 업로드가 끊기고 마지막 통계가 사라질 수 있습니다(`meta.db`는 SQLite 트랜잭션으로 쓰므로 깨지지 않습니다). 대기 시간을 60초로 두세요.

- compose: 들어 있는 `docker-compose.yml`에 `stop_grace_period: 60s`가 있습니다. 다른 compose 파일을 쓰면 서비스에 넣으세요.
- `docker run`으로 띄웠다면: `docker run --stop-timeout 60 …`으로 띄우거나 `docker stop -t 60 commandcenter`로 멈춥니다.
- Kubernetes: `terminationGracePeriodSeconds: 60`(차트의 기본값이고 참조 매니페스트에도 들어 있습니다)

## repolite에서 이전하기

이전 이름(repolite)으로 설치한 서버를 Command Center로 바꿀 때 확인할 것입니다. 데이터 형식(`dataDir`, 디스크 구조)은 바뀌지 않았습니다.

1. **이전 스택을 먼저 내리고 데이터 볼륨을 이어서 쓰기.** 순서가 중요합니다. 먼저 **이전 checkout 폴더에서** 이전 스택을 내립니다(compose 파일을 새 버전으로 받거나 바꾸기 전에).

   ```bash
   docker compose down --remove-orphans   # -v는 붙이지 마세요. 볼륨이 지워집니다
   ```

   그다음 compose 파일을 새 버전으로 받고, 쓸 버전을 `.env`의 `COMMANDCENTER_VERSION`에 적은 뒤("빠른 시작"의 2번) 볼륨 이름을 지정해 올립니다. compose의 기본 볼륨 이름이 `commandcenter-data`로 바뀌었으므로 그냥 `docker compose up`을 하면 빈 볼륨이 새로 생깁니다. 기존 데이터를 쓰려면 `COMMANDCENTER_VOLUME`에 이전 볼륨 이름을 지정하세요. 이름은 `docker volume ls`로 찾습니다. 보통 `repolite_repolite-data`(compose 프로젝트 이름 + 볼륨 이름)입니다.

   ```bash
   docker volume ls | grep repolite
   ```

   ```powershell
   # PowerShell: 현재 세션에서만 유효
   $env:COMMANDCENTER_VOLUME = "repolite_repolite-data"
   docker compose up -d
   ```

   ```bash
   # Git Bash / macOS / Linux
   COMMANDCENTER_VOLUME=repolite_repolite-data docker compose up -d
   ```

   매번 지정하기 번거로우면 `docker-compose.yml`과 같은 폴더의 `.env` 파일에 적어 두세요. compose가 자동으로 읽습니다.

   ```
   COMMANDCENTER_VERSION=1.2.3
   COMMANDCENTER_VOLUME=repolite_repolite-data
   ```

   이전 볼륨(`repolite_repolite-data`)을 지정해 올리면 `volume "…" already exists but was created for project "repolite"` 경고가 나올 수 있습니다. 데이터에는 영향이 없는 무해한 경고입니다. 이전 이름의 컨테이너가 남아 있으면 `docker rm -f repolite`로 정리합니다.
2. **설정 파일 경로.** 컨테이너 안의 설정 경로가 `/etc/repolite/config.yaml`에서 `/etc/commandcenter/config.yaml`로 바뀌었습니다. 새 경로에 파일이 없고 이전 경로에 있으면 이전 경로를 쓰고 시작할 때 경고를 로그에 남깁니다. 그래서 이전 compose 파일을 그대로 써도 동작하지만, `docker-compose.yml`의 마운트를 새 경로로 고치는 것을 권장합니다.
3. **토큰.** 이전에 발급한 `rl_` 토큰은 계속 동작합니다(Bearer, Cargo 토큰, Basic 비밀번호 자리 모두). 새로 발급하는 토큰은 `cc_`로 시작합니다. 클라이언트 설정을 바꿀 필요는 없습니다.
4. **UI 세션 초기화.** 세션 쿠키 이름이 `cc_session`(`baseURL`이 https면 `__Host-cc_session`)으로 바뀌고 세션은 원래 메모리에만 있으므로, 업그레이드하면 모두 로그아웃되어 한 번 다시 로그인해야 합니다. 로그인과 로그아웃 때 이전 `repolite_session` 쿠키는 만료시켜 지웁니다.
   - **클라이언트 설정의 id.** 웹 UI와 이 문서의 새 클라이언트 설정 예시는 서버 id와 레지스트리 이름으로 `commandcenter`를 씁니다. `repolite`를 쓰는 기존 `settings.xml`과 cargo 설정은 그대로 동작합니다. 다만 `settings.xml`에서는 `<server>`의 `<id>`와 저장소(`<repository>`, `<mirror>`)의 id가 서로 같아야 인증이 붙습니다. 새 이름으로 바꾸려면 둘을 함께 바꾸세요.
5. **이미지와 컨테이너 이름.** 이미지는 `ghcr.io/sungjinseo/commandcenter:<버전>`(직접 빌드했다면 `.env`의 `COMMANDCENTER_IMAGE`에 적은 이름), compose 서비스와 컨테이너는 `commandcenter`입니다. `docker logs commandcenter`, `docker exec commandcenter ...`처럼 이름을 쓰는 스크립트와 모니터링을 고치세요. 바이너리 서브커맨드(`commandcenter hash-password`, `token create`, `gc`, `healthcheck`)도 같은 이름입니다. `token create`는 `-config`를 생략하면 기본 경로(`/etc/commandcenter/config.yaml`)를 쓰고, 없으면 이전 경로로 대체하므로 예시에서는 `-config`를 쓰지 않습니다. 다른 경로의 파일을 쓸 때만 `-config <경로>`를 붙이세요.

## 웹 UI

브라우저에서 `http://localhost:8081/`(또는 `baseURL`)에 접속하면 `/ui/`로 이동합니다. 별도 설치나 외부 리소스 없이 서버에 내장되어 있어 폐쇄망에서도 동작합니다. 정적 파일 합계는 512 KiB 이하이며 바이너리에 포함됩니다. 화면 구조(사이드바, 상단 막대, 카드와 표)는 직접 디자인한 것이고 다른 제품의 로고, 아이콘, CSS 같은 자산은 쓰지 않습니다. 해시 경로는 `#browse/...`, `#admin/...` 형태입니다.

### 언어와 테마

- **언어 (KO/EN)**: 상단 막대의 KO/EN 버튼으로 한국어와 영어를 전환합니다. 선택은 브라우저에 저장됩니다. 저장된 값이 없으면 브라우저 언어가 영어(`en…`)일 때만 영어이고, 그 밖에는 한국어입니다. Hosted, Proxy, Group, Blob Store 같은 기술 용어는 두 언어 모두 영어로 둡니다. 서버가 돌려주는 오류 메시지는 번역하지 않고 영어 그대로 보여 줍니다.
- **테마**: 상단 막대의 테마 버튼을 누를 때마다 라이트, 다크, 시스템 순으로 바뀝니다. 기본값은 시스템(`prefers-color-scheme`)이며 선택은 브라우저에 저장됩니다.

### 화면 구성

- **상단 막대**: 현재 위치(브레드크럼), 컴포넌트 검색창(`/` 키로 포커스, 입력 중에는 동작하지 않음), 테마 버튼, 언어 버튼, 사용자 영역. 로그인 전에는 로그인 버튼이 있고 누르면 사용자 이름과 비밀번호 대화상자가 열립니다. 로그인 후에는 사용자 메뉴에서 계정, API 토큰, 로그아웃을 고릅니다. 화면 너비가 800px 이하이면 사이드바가 66px 폭의 아이콘 레일로 줄어들고 항목 이름은 툴팁으로 보입니다(상단 검색창과 포맷별 검색 하위 링크도 숨겨지며, 검색 화면에서 포맷을 고를 수 있습니다).
- **사이드바**: 아래 항목을 묶음별로 보여 주고, 권한이 없는 항목은 숨깁니다.
  - WORKSPACE: 대시보드, 저장소 탐색, 컴포넌트 검색, 컴포넌트 업로드(업로드 가능한 사용자만)
  - OPERATIONS(관리자): 저장소 설정, Blob Store, Cleanup 정책, 예약 작업
  - ACCESS CONTROL: 사용자·역할·Realm(관리자), API 토큰(로그인한 사용자)
  - SYSTEM(관리자): 일반 설정(이메일 알림 포함), 시스템 상태, 시스템 정보
  - 맨 위의 인스턴스 표시에 접속 호스트와 상태 점(초록/빨강)이 나오고, 맨 아래에 "COMMAND CENTER"와 "오픈소스" 문구가 나옵니다. 버전은 로그인한 사용자에게만 표시됩니다(`/-/api/me`의 `version`).
- 로그인하지 않아도 `anonymousRead`로 읽을 수 있는 저장소는 볼 수 있습니다. 읽을 수 없는 저장소는 목록에서 빠지고 직접 열어도 404입니다.

### 경로

| 경로 | 화면 | 권한 |
|---|---|---|
| `#browse/welcome` | 대시보드: 지표 카드, 주요 저장소, 저장 공간, 바로가기 | 누구나 |
| `#browse/search/generic=keyword%3D<q>` | 컴포넌트 검색: Keyword, Format, Repository | 읽기 가능한 저장소만 |
| `#browse/search/custom` | Custom search: Keyword, Format, Repository, Group, Name, Version | 〃 |
| `#browse/search/<fmt>` (`cargo`, `golang`, `maven`, `npm`, `pypi`) | 포맷별 검색 | 〃 |
| `#browse/browse` | 저장소 탐색: 저장소 표(형식, 이름, 유형, 상태, 파일 수, 크기)와 텍스트·포맷·유형 필터 | 〃 |
| `#browse/browse:<repo>` | 저장소 트리와 상세 패널 | 〃 |
| `#browse/browse:<repo>:<경로>` | 트리에서 해당 폴더·파일 선택 (경로는 `encodeURIComponent`) | 〃 |
| `#browse/upload` | 업로드: 올릴 수 있는 저장소 목록 | 쓰기 가능한 hosted maven/npm/pypi가 있는 사용자 |
| `#browse/upload/<repo>` | 해당 저장소의 업로드 폼 | 〃 |
| `#admin/repository/repositories[:<repo>]` | 저장소 설정 (목록, 상세, 편집, 삭제) | 관리자 |
| `#admin/repository/repositories:create` | 저장소 만들기: 레시피 선택(maven2, npm, pypi, go, cargo × hosted/proxy/group)과 폼 | 관리자 |
| `#admin/security/users[:<id>]` | 사용자 (읽기 전용) | 관리자 |
| `#admin/security/usertoken` | API 토큰: 전체 사용자 (관리자) | 관리자 |
| `#admin/support/status[:<check>]` | 시스템 상태 (상태 점검, 지표) | 관리자 |
| `#admin/support/systeminformation` | 시스템 정보 | 관리자 |
| `#admin/system/tasks[:<id>]` | 예약 작업 | 관리자 |
| `#user/account` | 계정 (사용자, `config.yaml`의 역할, 관리자 여부, 읽기 전용. 역할은 `/-/api/me`가 돌려줍니다) | 로그인 |
| `#user/usertoken` | API 토큰 (본인 토큰) | 로그인 |
| `#login` | 대시보드 위에 로그인 대화상자 | 누구나 |

이전 버전의 `#/...` 북마크는 새 경로로 자동 이동합니다: `#/` 은 `#browse/welcome`, `#/r/<저장소>` 와 `#/r/<저장소>/browse/<경로>` 는 Browse 트리, `#/search?q=` 는 일반 검색, `#/tokens` 는 `#user/usertoken`, `#/admin` 은 `#admin/support/status`, `#/login` 은 `#login`.

### Browse

저장소를 열면 왼쪽에 디렉터리 트리, 오른쪽에 상세 패널이 나옵니다(트리는 방향키, Home/End, Enter로 조작). 한 폴더에는 최대 500개까지 표시하고 넘으면 안내가 나옵니다. 상세 패널은 세 가지입니다.

- **저장소(루트)**: Summary(Format, Type, URL, Remote, Files, Size, Children)와 포맷별 **Repository setup**(Maven `settings.xml`, `.npmrc`, `pip.conf`, `GOPROXY`, `~/.cargo/config.toml` 등, 복사 버튼).
- **컴포넌트**: Summary(Repository, Format, Group, Name, Version), Usage(도구를 고르면 Maven/Gradle, npm, pip, go get, cargo add 같은 사용 예 복사), Assets(파일 목록과 크기). hosted 저장소에 `delete` 권한이 있으면 "Delete component"가 나옵니다("컴포넌트 삭제" 참고).
- **파일(asset)**: Summary(Path, Content type, File size, Blob created/updated, Blob reference(SHA-256), 프록시는 Locally cached), Usage, Attributes(checksum: sha1, md5, sha256, sha512 등)와 "Download". hosted 저장소에 `delete` 권한이 있으면 Maven은 "Delete asset"(그 파일만), 다른 포맷은 "Delete component"(그 파일이 속한 버전 전체)가 나옵니다.
- 폴더: Summary(Repository, Path, Children).

### 검색

헤더 검색창과 Search 화면은 같은 API(`GET /-/api/search/components`)를 씁니다. 기준 중 하나는 2자 이상이어야 합니다. 결과는 저장소, group, 이름, 버전 순으로 정렬해 한 페이지에 최대 100개 컴포넌트(컴포넌트당 파일 50개)씩 돌려줍니다. 다음 페이지가 있으면 응답에 `continuationToken`이 있고, 같은 조건에 `continuationToken=<그 값>`을 더해 다시 부르면 이어지는 100개를 받습니다(마지막 페이지에는 없고, 잘못된 값은 400). Search 화면에서는 표 아래의 "다음 100개 보기" 버튼이 다음 페이지를 표에 덧붙입니다. `truncated: true`는 이 응답에 일치하는 결과가 다 들어 있지 않다는 뜻이라 다음 페이지가 있을 때도 켜집니다. 마지막 페이지에서도 켜져 있으면 검색 한도에 걸렸거나 파일이 50개를 넘는 컴포넌트가 있는 것이고("컴포넌트는 파생 값" 참고), 화면에 "검색 한도에 걸려 일부 결과가 빠졌을 수 있습니다" 안내가 나옵니다. 결과 표의 열은 Name, Group, Version, Format, Repository, Last Updated이고, 행을 누르면 컴포넌트 상세(Summary, Usage, Assets)와 파일 상세로 들어갑니다. 일치하는 항목이 없으면 "No components matched the filter criteria"가 나옵니다.

- Generic/Custom: Keyword(group, name, version 중 어디든), Format, Repository, 그리고 More criteria(Group, Name, Version).
- Maven: Group Id, Artifact Id, Version, Base Version, Classifier, Extension.
- npm: Scope, Name, Version. PyPI, Go, Cargo: Name, Version.
- 이름 계열은 대소문자를 구분하지 않는 부분 일치입니다. Version은 `*`가 없으면 정확히 일치하고 `*`가 있으면 glob입니다.
- group 저장소는 검색하지 않습니다(멤버 저장소에서 찾습니다).

### 컴포넌트는 파생 값

Command Center는 파일만 저장하므로 컴포넌트는 서버가 요청 시점에 계산합니다(저장하지 않음).

- **Maven**: 경로 `g1/g2/…/artifact/version/파일` 에서 group `g1.g2…`, name `artifact`, version을 얻습니다. `maven-metadata.xml*` 은 컴포넌트가 아닙니다.
- **npm**: `이름/-/이름-버전.tgz`, scope 패키지는 `@scope/이름/-/…` (group `@scope`).
- **Go**: `모듈/@v/버전.(zip|mod|info)`.
- **Go**의 경로는 대소문자 인코딩(`!b` = `B`)을 쓰므로 모듈 이름과 버전은 풀어서 보여 줍니다(`github.com/!burnt!sushi/toml` → `github.com/BurntSushi/toml`).
- **PyPI, Cargo**: hosted 저장소는 packages 테이블의 이름과 버전. PyPI는 파일 이름에서 표시 버전을 뽑습니다. Cargo는 packages 테이블과 경로에 소문자로 저장한 이름 대신 올린 표기(대소문자 포함)를 보여 줍니다.
- **프록시 저장소(npm, PyPI, Cargo)**: packages 테이블에 행이 없으므로 캐시된 파일의 경로(assets 테이블)에서 같은 방식으로 파생합니다. Cargo는 경로에 이름이 소문자로 남으므로, 캐시해 둔 인덱스 파일(`index/<인덱스 경로>`) 첫 줄의 `name`에서 올린 표기를 읽어 보여 줍니다. 보여 주는 컴포넌트마다 그 파일의 앞부분(최대 4 KiB)만 읽고 원격에는 묻지 않습니다. 인덱스 파일이 캐시에 없거나 첫 줄의 첫 키가 `name`이 아니면(crates.io와 cargo는 늘 `name`을 먼저 씁니다) 경로의 소문자 이름을 씁니다.
- 검색은 메타 DB에서 한도까지만 훑습니다: assets 테이블 질의 한 번마다 최대 2000개 파일, packages 테이블은 이름 2000개와 버전 2000개까지 봅니다(`keyword`만 주면 이름으로 좁힌 검사를 먼저 하고, 한도가 남으면 버전 일치를 찾는 전체 검사를 이어서 합니다). 한도에 걸리거나 10초 안에 끝나지 않으면 그때까지의 결과를 `truncated: true`로 돌려줍니다. 페이지마다 같은 한도 안에서 처음부터 다시 훑고 정렬한 뒤 잘라 내므로, 데이터가 바뀌지 않는 한 페이지가 겹치거나 빠지지 않습니다(10초 제한에 걸린 경우는 예외). 대신 한도 밖의 결과는 페이지를 넘겨도 나오지 않으니 조건을 좁히세요. 검색은 무거운 질의용 세마포어 안에서 실행하므로 메모리 사용이 일정합니다.

### 컴포넌트 삭제

hosted 저장소에 `delete` 권한이 있으면 잘못 올라간 버전을 바로 지울 수 있습니다. npm packument와 dist-tag, PyPI 프로젝트 페이지, Go `@v/list`와 `@latest`, Cargo 인덱스가 바로 바뀌고 그 저장소의 파일 URL은 404가 됩니다. group은 멤버를 그때그때 읽으므로 그 저장소를 멤버로 둔 group에도 바로 반영됩니다. 삭제마다 활동(Events)에 `delete`가 남고(지운 사람, 대상, 지운 파일 수), blob 파일은 다음 GC(Compact blob store)에서 회수됩니다. `writePolicy: deny`(읽기 전용)이거나 Offline인 저장소에서는 지울 수 없습니다.

- **웹 UI**: 탐색과 검색의 컴포넌트 화면과 파일 화면에 "Delete component"가 나옵니다. Maven은 파일을 하나씩 `DELETE`하고, 파일 화면의 "Delete asset"으로 그 파일만 지울 수도 있습니다. npm·PyPI·Go·Cargo는 서버가 그 버전의 파일을 한 번에 모두 지웁니다(npm은 tarball, PyPI는 그 버전의 wheel과 sdist 전부, Go는 `.mod`·`.info`·`.zip`, Cargo는 `.crate`와 인덱스 줄). 화면 목록이 잘려 일부 파일이 보이지 않아도 모두 지웁니다.
- **API**: `DELETE /-/api/repositories/<저장소>/components?group=<group>&name=<이름>&version=<버전>`. `group`은 Maven의 groupId(필수)나 npm scope(`@scope`, 없으면 비움)이고, 다른 포맷은 비웁니다. `name`과 `version`은 컴포넌트 검색이 돌려주는 값입니다. 다른 쓰기 API처럼 `X-Command-Center-UI: 1` 헤더가 필요하며 API 토큰으로도 됩니다. 성공하면 200과 `{"assets": <지운 파일 수>, "bytes": <크기>}`입니다. `delete` 권한이 없으면 403, hosted가 아니거나 `writePolicy: deny`이거나 인자가 빠지면 400, Offline이면 503, 그런 버전이 없으면 404입니다.

  ```bash
  curl -u admin -X DELETE -H 'X-Command-Center-UI: 1' \
    'http://localhost:8081/-/api/repositories/pypi-hosted/components?name=demo-pkg&version=1.0'
  ```

- **클라이언트**: npm은 `npm unpublish <패키지>@<버전>`과 `npm unpublish <패키지> --force`("npm 클라이언트 설정" 참고), Go는 버전의 `.mod`·`.info`·`.zip` 중 하나에 보내는 `DELETE`("Go 모듈 설정" 참고)로 지웁니다. 둘 다 group으로 보내면 `writeMember`에서 지웁니다. twine과 cargo에는 삭제 명령이 없으므로 PyPI와 Cargo는 웹 UI나 API를 씁니다.
- 지운 버전과 같은 번호로 다시 올릴 수 있습니다. 하지만 내용이 다르면 이미 받아 둔 쪽의 체크섬(npm `package-lock.json`의 `integrity`, Go `go.sum`, `Cargo.lock`)과 맞지 않아 설치가 실패하므로 새 버전 번호를 쓰세요.
- proxy 저장소의 캐시는 이 기능으로 지우지 않습니다(Cleanup 정책을 쓰세요). 웹 UI와 API로는 group에서 지울 수 없으니 멤버 저장소에서 지우세요.

### 관리자 화면

- **Repositories**: 목록, 상세, 만들기·편집·삭제(아래 "저장소 관리"). Name, Format, Type, URL, Online(Online/Offline 전환), Blob Store, Remote, Members, 저장소 크기, 그리고 versionPolicy, writePolicy, metadataMaxAge, contentMaxAge, negativeCacheTTL(설정값).
- **Users**: 사용자 목록·만들기·편집·삭제·비밀번호 재설정·API 토큰 모두 폐기(User ID, Source, Roles, Status). LDAP 로그인 사용자는 최근에 로그인한 사용자만 `directory` 행으로 보입니다("인증과 권한", "Realm과 LDAP" 참고). 비밀번호 해시는 절대 내보내지 않습니다.
- **Roles**: 역할과 권한(대상 × 동작) 편집. 내장 `admin` 역할은 수정·삭제할 수 없습니다.
- **Realms**: Realm 순서와 켜기/끄기, LDAP 설정과 연결 테스트.
- **Account**: 로그인한 사용자 본인의 비밀번호 변경(로컬 사용자만).
- **Blob Store**: 이름 붙은 blob 저장소 만들기·삭제와 사용량("Blob Store" 참고).
- **General settings**: baseURL, 익명·인증 사용자 읽기, 세션 유휴 시간, 최대 업로드 크기, 기본 언어, 이메일(SMTP) 설정과 테스트 메일("일반 설정", "이메일 알림" 참고).
- **API 토큰**: 관리자는 전체 사용자의 토큰을 조회·폐기합니다(`#admin/security/usertoken`). 일반 사용자는 `#user/usertoken`)에서 본인 토큰을 발급·폐기합니다. 토큰 값은 발급 직후 한 번만 표시되며, `commandcenter token create` 대신 쓸 수 있습니다.
- **Status**: 상태 점검 Database, Blob store, Memory(90% 이상이면 비정상), Remote fetch slots, Repositories. Metrics에는 서버 상태(버전, 가동 시간, RSS, 힙, 고루틴, GC 횟수)와 5초마다 갱신되는 RSS 그래프, 그리고 blob 합계(개수와 총 용량, 같은 내용은 한 번만 셈)가 표시됩니다. 저장소별 용량은 Repositories 화면에 있습니다.
- **System Information**: `commandcenter-status`, `commandcenter-configuration`, `system-runtime`, `system-environment`(GOMEMLIMIT, HTTPS_PROXY 설정 여부, TZ만), `system-filestores`. "Download as JSON" 버튼이 있습니다.
- **Tasks**: 예약 작업 목록, 만들기·편집·삭제, "지금 실행"과 마지막 실행 로그(아래 "예약 작업"). 기본으로 `compact-blobstore` 작업이 하나 있습니다.

### 의도적으로 없는 것

- **Go, Cargo 업로드 화면**: Upload는 Maven, npm, PyPI만 지원합니다. Go와 Cargo는 `curl`(Go) 또는 `cargo publish`로 올립니다.
- **Tags**, 악성 패키지 위험 분석, 라이선스 관리 화면, 유료 에디션 전용 화면, SAML 등. Human Edition에는 유료 에디션이 없습니다.

### 주의할 점

- **세션은 메모리에만 있습니다.** 서버를 재시작하면 모두 로그아웃됩니다. 세션은 최대 1000개(사용자당 20개), 마지막 사용 후 유휴 시간(일반 설정의 `sessionIdleMinutes`, 기본 720분 = 12시간) 또는 로그인 후 7일이 지나면 만료됩니다. 자리가 없으면 만료된 세션을 먼저 지우고, 그다음 로그인하는 사용자 자신의 가장 오래 쓰지 않은 세션을 닫습니다. 그 사용자의 세션이 하나도 없을 때만 전체에서 가장 오래 쓰지 않은 세션을 닫으므로, 한 사용자가 로그인을 반복해도 다른 사용자의 세션을 계속 밀어내지는 못합니다. 비활성화하거나 삭제한 사용자, 비밀번호를 바꾼 사용자의 세션은 즉시 무효가 됩니다(`user reset-password`처럼 다른 프로세스가 DB를 바꾼 경우에는 5초 안에).
- 세션 쿠키는 `HttpOnly`, `SameSite=Strict`입니다. `baseURL`이 `https://`면 이름이 `__Host-cc_session`이고 `Secure`, `Path=/`에 `Domain`이 없어서, 같은 도메인의 다른 호스트가 이 쿠키를 심거나 가릴 수 없습니다. `http://`면 `cc_session`입니다. 현재 scheme의 이름만 인정하므로 `baseURL`을 http와 https 사이에서 바꾸면 UI에 다시 로그인해야 합니다. 같은 이름의 쿠키가 여러 개 오면(http에서는 같은 도메인의 다른 호스트가 끼워 넣을 수 있습니다) 살아 있는 세션을 가리키는 쿠키만 보고, 서로 다른 사용자의 세션이 섞여 있으면 어느 쪽도 인정하지 않습니다. 다시 로그인하면 요청에 실린 쿠키의 세션을 모두 끝내므로 풀립니다. 쓰기 요청(PUT/POST/DELETE)은 `X-Command-Center-UI: 1` 헤더가 있어야 쿠키가 인정됩니다. 작업 실행(Tasks의 Run)은 추가로 관리자이며 비밀번호 또는 세션으로 인증한 경우에만 됩니다(토큰으로는 불가). **https 리버스 프록시 뒤에서 운영하면 `baseURL`을 `https://…`로 설정하세요.** 그래야 쿠키에 `Secure` 플래그와 `__Host-` 접두사가 붙습니다.
- 쿠키는 `Authorization` 헤더가 없는 요청에서만 쓰이므로 `mvn`, `npm` 같은 클라이언트에는 영향이 없습니다.
- 삭제 버튼은 hosted 저장소에 `delete` 권한이 있고, 저장소가 Online이며 `writePolicy`가 `deny`가 아닐 때만 나옵니다(저장소 API의 `canDelete`). Maven은 파일 단위, 다른 포맷은 버전 단위로 지웁니다("컴포넌트 삭제" 참고).
- 목록은 모두 상한이 있습니다(디렉터리 500, 검색 한 페이지 100, 패키지 버전 1000, 토큰 1000). 넘으면 화면에 안내 문구가 나오고, 검색은 "다음 100개 보기"로 이어서 봅니다.

## 저장소 관리

관리자는 저장소 설정에서 저장소를 만들고, 고치고, 지울 수 있습니다. 서버를 재시작하지 않아도 바로 반영됩니다.

> **경고: `config.yaml`의 `repositories:`는 첫 시작 때 한 번만 읽습니다.** 저장소 정의는 `meta.db`(SQLite)로 옮겨지고, 그 뒤로는 `config.yaml`의 `repositories:`를 고쳐도 **무시**됩니다(시작할 때 파일 이름을 밝히는 경고 로그가 남습니다). 저장소는 UI(또는 `/-/api/repositories` API)로 바꾸세요. 사용자, 역할, 일반 설정도 첫 시작 때 한 번만 가져오며 이후 UI에서 관리합니다("업그레이드" 참고).

- **만들기**: "Create repository"를 누르면 레시피 선택 화면(`maven2`, `npm`, `pypi`, `go`, `cargo` 각각 hosted, proxy, group)이 나오고, 고르면 해당 유형의 폼이 열립니다. 검증은 `config.yaml`과 같고 오류는 폼 맨 위에 표시됩니다. 새 저장소 이름은 영문자와 숫자로 시작하고 영문자, 숫자, `.`, `_`, `-`만 쓸 수 있으며 최대 64자입니다. `create`는 예약어라 쓸 수 없습니다(이미 저장된 이름은 그대로 동작합니다).
- **편집**: `name`, `format`, `type`은 만든 뒤 바꿀 수 없습니다(폼에서 비활성). 나머지(remote, 인증, 최대 보관 시간, negative cache TTL, group 멤버와 순서, writeMember, versionPolicy, 배포 정책(writePolicy), anonymousRead, Online/Offline)는 고칠 수 있습니다. 저장소의 Blob Store(`blobStore`)는 만들 때만 고르며 바꿀 수 없습니다("Blob Store" 참고). proxy의 `remote`를 바꿔도 캐시된 파일은 유지되지만, 이전 remote에서 받은 "없음" 기록(negative cache)은 지워집니다.
- **삭제**: 저장소 이름을 직접 입력해야 삭제됩니다. 다음 규칙이 있습니다.
  - group의 멤버(또는 writeMember)인 저장소는 409로 거부하며 메시지에 group 이름이 나옵니다. group에서 먼저 빼세요.
  - 삭제하면 그 저장소의 정의와 자산, 패키지, dist-tag, negative cache 행이 한 트랜잭션으로 지워집니다. blob 파일은 다음 GC(Compact blob store)에서 회수됩니다.
  - 같은 트랜잭션에서 모든 역할의 그 저장소 권한(대상이 정확히 `repo:<이름>`인 것)도 지웁니다. 그래서 같은 이름으로 저장소를 다시 만들어도 이전 권한이 따라오지 않습니다. `*`, `format:<포맷>`, 다른 저장소 대상은 그대로이고, 권한이 모두 빠진 역할도 지우지 않습니다. 웹 UI의 삭제 확인 창에 권한이 빠질 역할이 나옵니다. 권한을 지운 역할이 있으면 저장소 이름과 역할을 적은 WARN 로그(`removed the privileges on a deleted repository from roles`) 한 줄과, 역할마다 활동 기록의 `role.update` 이벤트(`removed repo:<이름>`)가 남습니다. 저장소를 지웠다가 같은 이름으로 다시 만드는 스크립트는 역할에 권한을 다시 넣어야 합니다.
  - 이전 버전에서 저장소를 지울 때 남은 권한은 그대로 있고, 서버가 시작할 때 역할과 저장소마다 경고 로그(`role grants privileges on a repository that does not exist`)가 남습니다. 같은 이름으로 저장소를 다시 만들면 그 권한이 새 저장소에 적용되므로, 필요 없으면 역할에서 빼세요.
- **입력 검사**: 저장소를 만들거나 편집할 때(UI·API, 첫 시작의 config 가져오기) group의 멤버가 중복되면 거부하고(`duplicate member`), 시간 값(`metadataMaxAge`, `contentMaxAge`, `negativeCacheTTL`)에 음수(`-5m`)를 쓰면 거부합니다(400). "영원히"는 `-1`로만 씁니다. `remote`와 `remoteAuth`는 proxy에만 쓸 수 있고 `versionPolicy`는 group에 쓸 수 없습니다(400). 웹 UI의 Maven group 폼에는 versionPolicy 칸이 없습니다. 이전 버전이 이미 저장한 정의는 그대로 읽습니다. 음수 시간은 0(캐시하지 않음, 즉시 만료)으로, 중복 멤버는 한 번만 쓰고, hosted·group의 `remote`·`remoteAuth`와 group의 `versionPolicy`는 쓰이지 않던 값이라 버리며, 시작 시 저장소마다 경고를 한 줄 남깁니다. 그런 저장소가 있어도 서버는 시작하고 다른 저장소의 편집도 막지 않으며, 그 저장소를 편집할 때 값을 고치면 됩니다. 저장소를 만들거나 지우면 목록의 개수·크기 통계(약 30초 캐시)도 바로 다시 계산하므로, 같은 이름으로 다시 만든 빈 저장소가 이전 값을 보여 주지 않습니다.
- **원격 인증 정보**: proxy의 비밀번호와 Bearer 토큰은 DB에 저장되지만(비밀값 키를 정하면 암호화해서, "저장된 비밀값 암호화" 참고) **어떤 API와 화면에도 다시 보이지 않습니다**(설정되어 있는지 여부만 표시). 편집 때 비워 두면 기존 값을 유지하고, "clear" 체크로 지웁니다. `remote`의 **호스트가 바뀌면 인증 정보를 다시 입력해야 합니다**(토큰을 새 호스트로 옮길 수 없습니다).
- **권한**: 만들기, 편집, 삭제는 관리자이면서 비밀번호 또는 세션으로 인증한 경우에만 됩니다(API 토큰으로는 403). 쓰기 요청에는 `X-Command-Center-UI: 1` 헤더가 필요합니다.
- **백업**: 저장소 정의가 `meta.db`에 있으므로 **백업에 `meta.db`가 반드시 포함되어야 합니다**(데이터 볼륨 전체를 백업하면 됩니다). 정의 없이 blob만 복원하면 저장소가 없는 상태가 됩니다.

## 업로드

사이드바 Upload(또는 Browse의 "Upload component")에서 브라우저로 파일을 올릴 수 있습니다. 쓰기 권한이 있는 hosted Maven, npm, PyPI 저장소가 있을 때만 보입니다. 브라우저가 각 클라이언트와 같은 요청을 만들어 기존 저장소 엔드포인트로 보내므로 검증과 쓰기 정책(`writePolicy`)은 서버 규칙 그대로입니다.

| 포맷 | 필요한 것 |
|---|---|
| Maven | 파일 하나 이상(행마다 Classifier, Extension. 확장자는 파일 이름에서 채움), Group ID, Artifact ID, Version, Packaging. "Generate a POM file"을 켜면 최소 POM을 만들어 같이 올립니다. 선택한 파일 중 `.pom`이 있으면 좌표를 읽어 채우고 고정합니다. 파일마다 `PUT`하며 순서대로 올리다 첫 실패에서 멈춥니다. |
| npm | `.tgz` 하나. 브라우저가 압축을 풀어 `package/package.json`에서 이름과 버전을 읽어 보여 주고(1 MiB 제한), 서버도 저장한 tarball에서 `package.json`을 다시 읽어 검사합니다("npm 클라이언트 설정" 참고). `npm publish`와 같은 publish 문서로 올리므로 문서 한도 10 MiB가 적용되며 큰 tarball은 경고가 나옵니다. |
| PyPI | wheel 또는 sdist(`.tar.gz`, `.zip`) 하나. 이름과 버전을 파일 이름에서 읽으며 고칠 수 있지만, 서버가 파일 안의 메타데이터(`METADATA`, `PKG-INFO`)와 비교하므로 실제 패키지와 다르면 400입니다. `twine`과 같은 multipart 업로드이고 sha256은 https 또는 localhost(보안 컨텍스트)에서 접속했을 때만 브라우저가 계산하고, 그렇지 않으면 서버가 계산합니다. |

성공하면 "Upload successful"과 Browse 링크가 나옵니다. 이미 있는 파일을 다시 올리면 포맷별 규칙대로 오류가 인라인으로 표시됩니다(Maven `allow_once` 400, npm 403, PyPI 400). 파일은 순서대로 하나씩 올리며, 서버는 기존 스트리밍 경로를 그대로 쓰므로 큰 파일도 메모리에 통째로 올리지 않습니다(npm publish 문서만 10 MiB까지 읽고, 동시 처리는 2개로 제한됩니다). npm publish 슬롯이 모두 사용 중이면 30초까지 기다리다가 `503 too many concurrent publishes, retry`로 거절하므로 잠시 후 다시 시도하세요. 슬롯을 잡은 채 본문 전송이 5분 넘게 멈추면 연결이 끊깁니다. 다른 포맷의 업로드도 마찬가지로 본문을 읽는 중에 60초 동안 한 바이트도 오지 않으면(느려도 계속 오는 전송은 끊지 않습니다) **408**로 답하고(Cargo publish의 메타데이터 부분은 400입니다. Maven `maven-metadata.xml`은 계속 조금씩 오더라도 10초 안에 다 오지 않으면 408입니다) 연결을 끊어, 멈춘 업로드가 같은 경로의 잠금을 붙들고 있지 못하게 합니다. 서버가 본문을 읽기 전에 잠금을 기다리는 시간이나 본문이 없는 요청(DELETE, Cargo yank)은 이 제한에 들어가지 않습니다. 이 제한은 인증보다 먼저 본문이 있는 모든 요청(웹 UI의 `/-/api/…` 포함)에 걸리므로, 401·403·404·503으로 거절된 요청도 본문을 멈춘 채 연결을 붙들 수 없습니다. 서버가 끝까지 읽지 않은 본문은 연결을 다시 쓰려고 남은 부분(최대 256 KiB)을 최대 5초만 기다리고, 그 안에 다 오지 않으면 응답 뒤 연결을 닫습니다. 로그인하지 않은 요청의 본문(웹 UI 로그인, `npm login`)은 바이트가 계속 오더라도 요청 헤더를 받은 뒤 5초 안에 모두 와야 하며, 넘으면 요청을 거절하고 연결을 닫습니다. 익명으로는 업로드할 수 없으므로 인증된 요청인 업로드에는 이 제한이 없습니다.

**업로드가 401로 실패할 때**: 브라우저가 `/repository` 경로에 대해 Basic 인증 정보를 캐시해 두었다면(예: 그 주소를 직접 열어 로그인한 경우) 그 값이 UI 세션보다 우선하므로 업로드가 401로 실패할 수 있습니다. 브라우저에 저장된 해당 사이트의 인증 정보를 지우고 다시 시도하세요.

## 설정 레퍼런스

설정 파일은 YAML이며 알 수 없는 키가 있으면 시작이 실패합니다. 설정을 바꾼 뒤에는 `docker compose restart`로 반영하세요.

**한 번만 가져오는 키**: `users`, `roles`, `anonymousRead`, `baseURL`, `maxUploadMiB`는 **첫 시작 때 한 번만** 읽어 DB로 가져옵니다. 이후에는 파일에 남아 있어도 무시되며(시작할 때마다 경고가 한 줄 남습니다) 웹 UI에서 관리합니다("업그레이드"의 이전 안내 참고). `config.yaml`에 계속 쓰는 키는 `listen`, `dataDir`, `memoryLimitMiB`, `metricsFlushSeconds`, `secretKeyFile`뿐입니다(`repositories`도 첫 시작에만 가져옵니다).
전체 예시는 `config.example.yaml`을 참고하세요.

### 최상위 키

| 키 | 타입 | 기본값 | 설명 |
|---|---|---|---|
| `listen` | 문자열 | `":8081"` | 수신 주소. 컨테이너 안에서는 8081을 유지하세요(헬스체크가 127.0.0.1:8081을 확인). |
| `baseURL` | URL | `"http://localhost:8081"` | (가져온 뒤 UI의 일반 설정에서 관리) 클라이언트가 접속하는 주소. npm·Cargo 응답의 다운로드 URL이 이 값으로 만들어집니다(PyPI 파일 링크는 호스트 없는 경로이며 이 값의 경로 부분만 앞에 붙습니다). 끝의 `/`는 무시되고, 사용자 정보(`user:pass@`), 쿼리(`?…`), 프래그먼트(`#…`)가 없는 `http(s)` URL이어야 합니다. 이 값은 클라이언트에게 주는 URL의 앞부분이 되므로 사용자 정보가 있으면 그 자격 증명이 모든 클라이언트에게 넘어가고, `?`나 `#`가 있으면 URL이 깨집니다. |
| `dataDir` | 경로 | `"/data"` | blob과 `meta.db`(SQLite)를 저장하는 디렉터리. compose에서는 named volume입니다. |
| `memoryLimitMiB` | 정수 | `80` | Go 메모리 한도(MiB). 환경변수 `GOMEMLIMIT`가 있으면 무시됩니다(compose 기본값은 `GOMEMLIMIT: 80MiB`). |
| `maxUploadMiB` | 정수 | `1024` | (가져온 뒤 UI에서 관리, 바로 적용) GET/HEAD가 아닌 요청 본문의 최대 크기(MiB). 허용 범위 1..1048576, 0이면 기본값. 초과하면 모든 포맷에서 413. 포맷별로 더 작은 제한이 있으면 그것이 먼저 적용됩니다. |
| `anonymousRead` | 불리언 | `false` | (가져온 뒤 UI에서 관리) 인증 없는 다운로드 허용 여부. 저장소별로 덮어쓸 수 있습니다. |
| `metricsFlushSeconds` | 정수 | `30` | (숨은 설정) 다운로드 수와 요청 통계를 DB에 반영하는 주기(초). 허용 범위 1..3600. "다운로드 수·요청 추이" 참고. |
| `secretKeyFile` | 경로 | 없음 | 저장된 비밀값(SMTP 비밀번호, LDAP bind 비밀번호, proxy `remoteAuth`)을 암호화하는 키가 든 파일. 환경변수 `COMMANDCENTER_SECRET_KEY`로 줄 수도 있으며, 둘 다 주면 시작하지 않습니다. 없으면 비밀값을 평문으로 저장합니다. "저장된 비밀값 암호화" 참고. |
| `users` | 목록 | 없음 | (첫 시작 가져오기 전용) 사용자: `name`, `passwordHash`(bcrypt), `roles`. 가져온 사용자는 로컬 사용자가 됩니다. bcrypt 해시가 아닌 값(예: `config.example.yaml`의 `<commandcenter hash-password 출력값>`)은 비밀번호가 없는 것으로 봅니다. 그 사용자는 비밀번호로 로그인할 수 없고, 사용자 API의 `hasPassword`는 `false`이며, 서버가 시작할 때마다 그런 활성 사용자마다 WARN 로그(`local user cannot sign in with a password`)가 남습니다. 웹 UI나 `user reset-password`로 비밀번호를 정하세요. |
| `roles` | 맵 | 없음 | (첫 시작 가져오기 전용) 역할 이름 → `write: [저장소 이름…]`. `"*"`는 모든 저장소. 가져올 때 항목마다 `repo:<이름>`(또는 `*`) 대상에 browse·read·add·edit·delete 전부를 주는 권한이 됩니다(예전의 쓰기는 삭제를 포함했습니다). |
| `repositories` | 목록 | 없음 | 저장소 정의(아래 표). **첫 시작 때만** 읽어 DB로 가져옵니다. 이후에는 무시되므로 UI에서 관리합니다("저장소 관리" 참고). |

`admin` 역할은 내장되어 있어 `roles`에 정의하지 않아도 되며 모든 저장소의 모든 동작과 관리 화면, `/-/metrics` 조회가 가능합니다. `roles`에 `admin`을 적어도 무시하고 내장 역할을 씁니다.

### 저장소 키

아래 키는 UI 폼과 `config.yaml`(첫 시작 가져오기)에서 같은 의미입니다.

| 키 | 대상 | 기본값 | 설명 |
|---|---|---|---|
| `name` | 모두 | 필수 | 저장소 이름. URL의 `/repository/<name>/`. `/`, 공백, `\` 불가, 중복 불가. UI/API로 만들 때는 영문자·숫자로 시작하고 영문자, 숫자, `.`, `_`, `-`만, 최대 64자, `create`는 예약어. |
| `format` | 모두 | 필수 | `maven`, `npm`, `pypi`, `go`, `cargo` 중 하나. |
| `type` | 모두 | 필수 | `hosted`, `proxy`, `group` 중 하나. |
| `remote` | proxy | 필수 | 원격 저장소의 `http(s)` URL. Cargo는 `sparse+` 없는 sparse index URL. 원격 인증 정보는 URL에 넣지 말고 `remoteAuth`에 넣으세요(URL은 암호화하지 않습니다. "저장된 비밀값 암호화" 참고). |
| `remoteAuth` | proxy | 없음 | 원격 인증: `username`/`password` 또는 `bearerToken`(둘 다 있으면 토큰 우선). 설정한 `remote`와 스킴·호스트·포트가 같은 곳에만 보내며, 리다이렉트를 따라갈 때도 같습니다. 예외는 같은 호스트의 `http`(80)에서 `https`(443)로 옮기는 리다이렉트로, 첫 요청이 이미 그 호스트에 보냈으므로 자격 증명을 유지합니다. 그 밖에 포트나 스킴이 다른 곳(`https`에서 `http`로 내려가는 경우 포함)과 하위 도메인으로 가는 리다이렉트는 자격 증명 없이 따라갑니다. |
| `metadataMaxAge` | proxy | `"24h"` | 메타데이터(maven-metadata.xml, packument, simple 인덱스, Go `list`/`@latest`, Cargo 인덱스)를 다시 확인하기까지의 시간. |
| `contentMaxAge` | proxy | `"-1"` | 아티팩트 본문(jar, tgz, 휠, zip, crate)의 재확인 주기. 기본은 영구 캐시. |
| `negativeCacheTTL` | proxy | `"10m"` | 원격이 404/410을 준 경로, 그리고 체크섬이 맞지 않은 다운로드를 다시 묻지 않는 시간. 만료된 기록은 서버가 시작할 때와 그 뒤 하루에 한 번 DB에서 지웁니다. |
| `members` | group | 필수 | 멤버 저장소 이름 목록. 순서대로 조회하며 같은 포맷이어야 하고 순환할 수 없습니다. |
| `writeMember` | group | 없음 | 업로드를 받을 hosted 멤버(`members`에 포함되어야 함). 없으면 group에 쓰기는 405. |
| `versionPolicy` | Maven hosted | `release` | `release`, `snapshot`, `mixed`. Maven에서만 쓸 수 있습니다. |
| `writePolicy` | hosted | `allow_once` | `allow`(덮어쓰기 허용), `allow_once`(같은 경로 재업로드 거부), `deny`(읽기 전용). 아래 설명 참고. |
| `anonymousRead` | 모두 | 최상위 값 | 이 저장소만 익명 읽기를 켜거나 끕니다. |
| `cleanupPolicy` | hosted, proxy | 없음 | 연결할 Cleanup 정책 이름 하나(없으면 비움). 정책의 포맷이 `all`이거나 저장소와 같아야 하며 Cargo hosted에는 쓸 수 없습니다("Cleanup 정책" 참고). |
| `blobStore` | hosted, proxy | `default` | 이 저장소의 blob을 둘 blob store 이름("Blob Store" 참고). 만들 때만 정할 수 있고 바꿀 수 없으며 group에는 없습니다. 없는 store 이름은 거부됩니다. |
| `online` | 모두 | `true` | `false`이면 저장소가 Offline이 되어 요청에 503을 돌려줍니다("저장소 Online/Offline" 참고). |

**기간 형식**: Go 기간 문자열(`"30m"`, `"1h"`, `"24h"`)이며 `d` 단위는 없습니다. `"-1"`은 만료 없음(영구)입니다. 생략하거나 `0`이면 기본값입니다.
원격에 접속할 수 없을 때는 만료된 캐시가 있으면 그대로 제공하고 `Warning: 110` 헤더를 붙입니다. 원격이 응답 헤더를 보낸 뒤, 본문을 기다리는 서버에 60초 동안 한 바이트도 보내지 않을 때도 받기를 중단하고 같은 방식으로 처리합니다. 받은 내용을 blob store에 쓰는 시간은 이 60초에 세지 않으므로, blob store의 디스크(NFS 등)가 잠시 멈춰도 원격이 정상이면 받기가 끊기지 않습니다. 느려도 계속 오는 전송은 끊지 않으며, 한 번 받기의 전체 상한은 30분입니다.

**원격 접속 제한**: proxy는 리다이렉트를 5번까지만 따라가고, 접속하는 순간의 실제 IP를 검사합니다. 그래서 DNS 응답을 중간에 바꾸는 방법(DNS rebinding)으로도 우회할 수 없습니다. 무엇을 허용할지는 `remote` 호스트가 가리키는 주소로 정합니다.

- `remote`가 공인 주소이면, 리다이렉트나 원격 인덱스가 준 URL(PyPI 파일 URL, Cargo `config.json`의 `dl`)이 다음 주소로 향할 때 거절합니다: 사설(10/8, 172.16/12, 192.168/16, 100.64/10, fc00::/7), loopback, link-local(169.254.169.254 같은 클라우드 메타데이터 포함), 미지정(`0.0.0.0`, `::`), 멀티캐스트.
- `remote`가 사설 주소인 사내 저장소(예: 사내 Nexus)에는 그대로 접속하고, 다른 사설 호스트로 가는 리다이렉트도 따라갑니다.
- loopback, link-local, 미지정, 멀티캐스트 주소는 `remote` 자체가 그런 주소일 때만 허용합니다(예: `http://127.0.0.1:8082/`).
- NAT64 주소(`64:ff9b::/96`, `64:ff9b:1::/48`)는 마지막 32비트에 담긴 IPv4 주소로 판단합니다. 예를 들어 `64:ff9b::a00:5`는 `10.0.0.5`와 같은 사설 주소로 봅니다. `64:ff9b:1::/48` 안에서 /96보다 짧은 변환 접두사를 쓰거나 통신사·사내에서 따로 정한 NAT64 접두사를 쓰는 망에서는 이 판단이 맞지 않으므로, NAT64 게이트웨이나 방화벽에서 내부 대역으로 가는 변환을 막으세요.
- IPv4 주소는 점으로 구분한 네 부분의 십진수(`10.0.0.1`)로 적어야 합니다. `10.1`, `167772161`, `0x0a000001`, `012.0.0.1`, `10.0.0.1.`처럼 호스트의 마지막 부분이 숫자인 다른 표기는 많은 리졸버와 사내 프록시가 IPv4 주소로 읽으므로, `remote`가 사내 저장소여도 항상 거절합니다. `remote` 자체를 이렇게 적어도 거절됩니다.
- 호스트 이름은 ASCII로만 적습니다. 국제화 도메인(예: `bücher.example`)은 퓨니코드(`xn--bcher-kva.example`)로 적으세요. ASCII가 아닌 호스트는 접속할 때 ASCII로 바뀌어 위 검사를 비켜 갈 수 있으므로(전각 문자로 쓴 `１０．１`은 `10.1`이 됩니다) `remote`든 리다이렉트나 원격 인덱스가 준 URL이든 항상 거절합니다. UI·API로 저장소를 만들거나 편집할 때 `remote` 호스트가 ASCII가 아니면 400이고, 첫 시작에 `config.yaml`에서 가져올 때는 시작이 실패합니다. 이전 버전이 저장한 그런 저장소가 있어도 서버는 시작하지만 그 저장소의 원격 요청은 모두 거절되므로 `remote`를 퓨니코드로 고치세요.
- 직접 접속할 때 `remote` 호스트가 가리키는 주소는 요청마다 DNS로 다시 확인합니다. 조회가 실패하면 최근 10분 안에 성공한 마지막 조회 결과를 대신 쓰므로, DNS가 잠시 응답하지 않아도 사내 저장소가 공인 저장소로 취급되어 거절되지 않고 이미 열린 연결도 계속 쓸 수 있습니다. 그런 결과가 없으면 `remote`를 공인 주소로 보고 가장 엄격하게 제한합니다.
- 거절되면 클라이언트는 502(만료된 캐시가 있으면 그 캐시)를 받습니다. 로그의 `remote fetch failed` 줄에는 `address refused by the remote egress policy`가 남습니다.
- `HTTPS_PROXY`/`HTTP_PROXY`의 사내 프록시를 거치는 요청은 프록시가 대신 접속하므로 IP 주소로 적힌 목적지만 검사합니다(위의 다른 IPv4 표기와 ASCII가 아닌 호스트의 거절 포함). 호스트 이름으로 적힌 목적지는 내부 주소로 해석되더라도 프록시가 그대로 접속하므로, 프록시에서 내부 대역으로 가는 요청을 막거나 사내 도메인과 대역을 `NO_PROXY`에 넣으세요. 프록시 자체의 주소는 막지 않습니다. `NO_PROXY`에 넣은 호스트에는 직접 접속하므로 위 검사를 모두 받습니다.

**writePolicy의 적용 범위**: Maven은 세 값을 모두 따릅니다. Go는 `allow`가 아니면 같은 파일을 거부하고 `deny`는 업로드를 막습니다. PyPI와 Cargo는 `deny`만 효과가 있으며 같은 파일·버전 재업로드는 항상 거부합니다. npm은 `writePolicy`를 쓰지 않으며 이미 게시된 버전은 항상 거부합니다. 단, `deny`인 저장소에서는 모든 포맷의 삭제(Maven `DELETE`, `npm unpublish`, Go `DELETE`, 웹 UI·API의 컴포넌트 삭제)가 400입니다. Cleanup 정책은 `writePolicy`를 무시합니다.

## 인증과 권한

사용자, 역할, Realm, LDAP 설정은 `config.yaml`이 아니라 **DB에서 웹 UI로 관리**합니다(첫 시작 때 `config.yaml`의 `users`/`roles`를 한 번 가져옵니다. "업그레이드" 참고). 변경은 바로 적용되며 재시작이 필요 없습니다.

### 사용자

- **Basic 인증**: 사용자 이름과 비밀번호. UI에서 사용자를 만들 때 비밀번호(8자 이상, 72바이트 이하, 공백만으로는 안 됨)를 입력합니다. `commandcenter hash-password`는 첫 시작 때 `config.yaml`에서 가져올 해시를 만들며 같은 비밀번호 규칙을 적용합니다(가져온 뒤에는 `config.yaml`의 사용자를 고쳐도 반영되지 않습니다). 관리자가 모두 잠겼을 때는 "비상 복구"의 `user reset-password`를 쓰세요. 사용자 ID는 영문자·숫자로 시작하고 영문자, 숫자, `.`, `_`, `@`, `-`만, 최대 64자이며 `create`는 예약어입니다. 이미 있는 ID와 대소문자만 다른 ID(예: `admin`이 있을 때 `Admin`)는 만들 수 없고, 409 응답에 기존 ID가 나옵니다(`already exists as "admin"`). 로그인은 대소문자를 구분하므로 비슷해 보이는 다른 계정이 생기지 않게 하기 위해서이며, 이전 버전이 이미 만든 그런 ID는 그대로 둡니다. 첫 가져오기에서도 같은 규칙을 적용하므로 규칙에 맞지 않는 ID(예: `:`이 들어간 ID)나 대소문자만 다른 ID 두 개가 `config.yaml`에 있으면 가져오기가 실패하고 서버가 시작되지 않습니다. 이름과 이메일에는 줄바꿈 같은 제어 문자를 쓸 수 없고, 이메일은 비워 두거나 `user@example.com` 형식의 주소 하나(표시 이름이나 여러 주소 없이)여야 합니다(400). 이전 버전이 저장한 값은 바꾸지 않는 한 다시 검사하지 않으므로 그런 사용자도 편집하거나 비활성화할 수 있습니다.
- **상태**: `active` 또는 `disabled`. 비활성화하거나 삭제한 사용자는 Basic 인증, 세션, 토큰 모두 곧바로 거부됩니다. 사용자를 삭제하면 그 사용자의 API 토큰도 함께 지워집니다.
- **본인 비밀번호 변경**: 로그인한 로컬 사용자는 계정 화면(`PUT /-/api/account/password`)에서 현재 비밀번호를 확인한 뒤 바꿉니다. 비밀번호나 세션으로 인증해야 하고 토큰으로는 안 됩니다. 새 비밀번호가 현재 비밀번호와 같으면 400입니다. 바꾸면 그 사용자의 세션이 모두 끊기고 현재 브라우저만 새 세션을 받습니다. LDAP 사용자는 비밀번호를 디렉터리에서 관리하므로 400입니다. 현재 비밀번호는 로컬 Realm으로만 확인하므로 같은 이름의 LDAP 사용자의 비밀번호로는 바꿀 수 없습니다. 관리자는 다른 로컬 사용자의 비밀번호를 재설정할 수 있고, 그러면 그 사용자의 세션이 모두 끊깁니다. 재설정은 새 비밀번호가 현재 비밀번호와 같은지 보지 않습니다(관리자가 사용자의 현재 비밀번호를 맞혀 볼 수 없게 하기 위해서입니다). 어느 쪽이든 API 토큰은 그대로 유효하므로, 비밀번호가 새어 나갔다고 의심되면 사용자 화면의 **토큰 모두 폐기**도 누르세요(아래 "API 토큰").
- **API 토큰**: UI의 API 토큰 화면 또는 `docker compose exec commandcenter commandcenter token create -user ci`가 `cc_…` 토큰을 만듭니다. `Authorization: Bearer <토큰>`, 토큰 값만 보내는 형태(Cargo), 또는 **Basic 인증의 비밀번호 자리**(사용자 이름은 토큰 소유자와 같아야 하며 다르면 401)로 쓸 수 있어 Maven·pip·Go CI에서 비밀번호 대신 넣을 수 있습니다. 토큰으로는 새 토큰을 발급·폐기하거나 웹 UI에 로그인할 수 없습니다(실제 비밀번호나 로그인 세션만 가능). 토큰은 만료되지 않지만 사용자를 비활성화·삭제하거나 `token` Realm을 끄면 무효가 됩니다. 비밀번호를 바꾸거나 재설정해도 토큰은 남습니다. 관리자는 사용자 화면의 **토큰 모두 폐기**(`DELETE /-/api/users/<id>/tokens`, 관리자이며 비밀번호 또는 세션으로 인증하고 `X-Command-Center-UI: 1` 헤더 필요)로 그 사용자의 토큰을 한 번에 폐기합니다. 같은 ID의 LDAP 사용자 토큰(`ldap:<id>`)도 함께 폐기하고, 응답 `{"revoked": <개수>}`와 활동 기록의 `token.revoke` 이벤트(`all <개수>`)에 폐기한 개수가 나옵니다. 폐기된 토큰을 쓰던 빌드와 CI는 401을 받으므로 새 토큰을 넣으세요. 토큰은 해시만 저장되므로 분실하면 다시 발급하세요. LDAP 사용자의 토큰은 소유자가 `ldap:<id>`입니다("Realm과 LDAP" 참고).
- **`npm login`**: 사용자 이름과 비밀번호로 로그인하면 서버가 토큰을 발급해 `~/.npmrc`에 저장합니다.
- **세션**: 웹 UI 로그인 세션은 메모리에만 있고 모두 1000개, 사용자당 20개까지입니다. 넘으면 그 사용자의 세션 가운데 가장 오래 쓰지 않은 것이 끊깁니다("웹 UI"의 "주의할 점" 참고).

### 역할과 권한

역할은 **권한(privilege)** 목록입니다. 권한 하나는 **대상**과 **동작**의 묶음입니다.

- **대상**: `*`(모든 저장소), `format:<포맷>`(`maven`, `npm`, `pypi`, `go`, `cargo`), `repo:<저장소 이름>`.
- **동작**: `browse`, `read`, `add`, `edit`, `delete`. 여러 권한과 여러 역할은 합쳐지며(어느 하나라도 허용하면 허용) 거부 규칙은 없습니다.
  - **browse**: UI의 저장소 목록, 탐색, 파일 정보, 검색, 컴포넌트 검색. 내용 다운로드(`/repository/`)는 안 되며 파일 화면의 Download 버튼도 나오지 않습니다. `read`가 있으면 browse도 됩니다.
  - **read**: 다운로드(GET/HEAD)와 메타데이터 조회.
  - **add**: 새 파일·버전 올리기. Maven `PUT`(새 경로), npm `publish`(새 버전), PyPI `twine upload`, Go 업로드, Cargo `publish`, UI 업로드.
  - **edit**: 이미 있는 것을 바꾸기. `writePolicy: allow`인 Maven·Go 저장소에서 같은 경로 덮어쓰기, npm `dist-tag`와 `deprecate`, Cargo `yank`/`unyank`. (`writePolicy`가 `allow_once`이거나 PyPI·Cargo처럼 재업로드를 항상 거부하는 포맷에서는 edit이 있어도 덮어쓸 수 없습니다.) Maven·Go 덮어쓰기에는 add와 edit이 모두 필요합니다(모든 Maven `PUT`과 Go 업로드는 add를 검사하고, 이미 있는 경로를 바꿀 때 edit을 한 번 더 검사합니다). npm `dist-tag`·`deprecate`와 Cargo `yank`/`unyank`는 edit만 있으면 됩니다. 역할 편집 화면에도 이 안내가 나옵니다.
  - **delete**: 삭제. Maven `DELETE`(파일), `npm unpublish`, Go `DELETE`(버전 전체), 웹 UI와 API의 컴포넌트 삭제(모든 포맷, "컴포넌트 삭제" 참고). `add`만 있으면 403입니다.
- group에 쓰는 요청은 같은 동작을 `writeMember` 저장소에 대해 검사합니다.
- **내장 `admin` 역할**은 모든 대상에 모든 동작을 가지며 관리 화면과 `/-/metrics`도 쓸 수 있습니다. 수정·삭제할 수 없습니다.
- 역할을 지우려는데 사용자가 쓰고 있거나 LDAP 설정의 `groupMappings[].roles` 또는 `defaultRoles`에 들어 있으면 409입니다(LDAP 참조는 메시지에 어느 매핑인지 나옵니다).
- **입력 검사**: `repo:<이름>` 대상은 그 이름의 저장소가 있어야 하며, 없으면 400(`unknown repository "<이름>"`)입니다. 이미 저장된 역할에 있던 대상은 저장소가 없어도(이전 버전이 저장소를 지울 때 남긴 권한 등) 그 역할을 편집할 때 그대로 둘 수 있습니다(새로 넣는 대상만 검사합니다). 저장소를 지우면 그 대상도 역할에서 지워지므로("저장소 관리"의 삭제 참고), 삭제 전에 열어 둔 역할 편집 화면을 그대로 저장하면 400입니다. 한 역할에 같은 대상을 두 번 쓰면 400(`duplicate target`)입니다. 역할 ID도 사용자 ID처럼 이미 있는 ID와 대소문자만 다른 ID는 만들 수 없고(409, `config.yaml` 첫 가져오기에서는 시작 실패), `admin`은 대소문자와 관계없이 예약어입니다. 설명에는 제어 문자를 쓸 수 없습니다. 이전 버전이 저장한 설명은 바꾸지 않는 한 다시 검사하지 않습니다.
- 예: `repo:maven-releases`에 `browse`, `read`, `add`만 있는 역할의 사용자가 Maven에 `PUT`하면 201, `DELETE`하면 403입니다.

### 읽기 권한

- **`authenticatedRead`**(기본 켜짐): 켜져 있으면 로그인한 사용자는 권한 없이도 모든 저장소를 읽고 browse할 수 있습니다. 끄면 로그인한 사용자도 `read`(UI 목록은 `browse` 또는 `read`) 권한이 있어야 합니다. 쓰기(add/edit/delete)는 항상 권한이 필요합니다.
- **익명 읽기**: 일반 설정의 `anonymousRead`(저장소별 `anonymousRead`가 있으면 그 값이 우선)가 켜진 저장소는 인증 없이 읽을 수 있습니다. group의 읽기 검사는 멤버가 아니라 **group 자신의** 값을 따릅니다. 익명 사용자의 쓰기는 항상 거부됩니다.
- **응답 코드**: 인증이 필요한데 없으면 401(`WWW-Authenticate` 포함), 인증은 됐으나 권한이 없으면 403. 잘못된 자격 증명은 `anonymousRead`가 켜져 있어도 401이고, 로그인 시도 제한에 걸린 이름은 429입니다("보호" 참고). 저장소가 Offline이면 권한 검사 **뒤에** 503입니다("저장소 Online/Offline" 참고).
- **없는 저장소**: 그 이름의 저장소를 읽을 수 없는 호출자(없는 이름에는 전역 `anonymousRead`와 `authenticatedRead`가 적용됨)에게는 읽을 수 없는 저장소와 똑같은 401/403으로 답하므로, 응답으로 숨은 저장소 이름을 알아낼 수 없습니다. 읽을 수 있는 호출자에게는 404입니다. 남는 예외: 전역 익명 읽기가 켜져 있을 때 저장소별 `anonymousRead: false`로 빠진 저장소는 익명 사용자에게 401이라 없는 저장소(404)와 구별되고, npm 저장소는 토큰 없이 써야 하는 로그인 경로가 인증 없이 답합니다.

### 잠금 방지

"로그인할 수 있는 관리자"가 없어지는 변경은 409로 거부됩니다. 관리자가 로그인할 수 있는 경로는 (a) `local` Realm이 켜져 있고 쓸 수 있는 비밀번호(bcrypt 해시)가 있는 활성 로컬 `admin` 사용자가 있거나, (b) `ldap` Realm이 켜져 있고 설정이 완전하며 그룹 매핑 또는 기본 역할이 `admin`을 주는 경우입니다. 대상이 되는 변경은 사용자 삭제·비활성화·역할 변경, `local`/`ldap` Realm 끄기, LDAP 매핑 편집입니다. 추가로 **마지막 로컬 관리자(활성, bcrypt 비밀번호 해시 있음, `admin` 역할)는 LDAP 관리자 매핑이 있어도 삭제·비활성화·강등할 수 없습니다.** 로컬 비상용 관리자를 항상 남기기 위해서입니다. 그래서 LDAP만 쓰는 설치에도 로컬 관리자 한 명은 남습니다(`local` Realm 자체는 끌 수 있습니다). placeholder처럼 bcrypt 해시가 아닌 `passwordHash`를 가진 관리자는 로그인할 수 없으므로 세지 않습니다. 그래서 그런 관리자만 남게 되는 삭제·비활성화·강등도 409입니다.

사용자, 역할, Realm, LDAP 설정의 변경은 서버 안에서 하나씩 차례로 처리합니다(읽기 → 잠금 방지 검사 → 저장 → 적용). 그래서 두 관리자가 동시에 서로를 비활성화해도 하나는 409를 받고 관리자 한 명은 남습니다.

### 비상 복구(break-glass)

모든 관리자가 잠겼거나(비밀번호 분실, 비활성화, `local` Realm 꺼짐) 웹 UI로 들어갈 수 없을 때는 DB를 직접 고칩니다. 서버를 멈출 필요는 없습니다.

```bash
# TTY에서 직접 입력하면 비밀번호가 화면에 그대로 보이므로 파이프로 넘기세요(8자 이상, 72바이트 이하)
# 서버가 실행 중일 때
printf '%s\n' "$PW" | docker compose exec -T commandcenter commandcenter user reset-password -user admin -admin
# 서버가 멈춰 있을 때
printf '%s\n' "$PW" | docker compose run --rm -T commandcenter user reset-password -user admin -admin
```

`user reset-password`는 표준 입력에서 비밀번호를 한 줄 읽고(API와 같은 규칙), 한 트랜잭션으로 다음을 바꾼 뒤 바꾼 내용을 출력합니다.

- 사용자의 비밀번호를 설정하고 상태를 `active`로 바꿉니다. 사용자가 없으면 로컬 사용자로 만듭니다. 대소문자만 다른 사용자가 이미 있으면(예: `Admin`이 있는데 `-user admin`) 만들지 않고 `already exists as "Admin"` 오류로 끝나므로(0이 아닌 코드) 나온 ID를 그대로 쓰세요. 이미 있는 `source=ldap` 덮어쓰기 행의 ID이면, 관리자가 비활성화한 디렉터리 계정이 되살아나거나 그 역할이 비밀번호 로그인으로 옮겨지는 일을 막기 위해 `user "<id>" is an LDAP role-override record; choose another id for a local recovery account` 오류로 거부하고 아무것도 바꾸지 않습니다(0이 아닌 코드로 종료). 이때는 다른 ID를 쓰세요.
- `-admin`을 주면 `admin` 역할을 더합니다.
- `local` Realm을 켭니다. Realm 목록에 없으면 맨 앞에 넣습니다.

첫 시작 전이라 `config.yaml`을 아직 가져오지 않았다면 가져오기를 먼저 합니다. `-config`는 `token create`와 같습니다. 실행 중인 서버는 사용자·역할·Realm·설정을 5초마다 DB에서 다시 읽으므로 바뀐 내용이 몇 초 안에 반영되며 재시작할 필요가 없습니다. 웹 UI에서 바꿀 때와 같이 비밀번호가 바뀐 사용자의 기존 세션은 끝나고 그 이름의 로그인 시도 제한(아래 "보호")도 풀리며, API 토큰은 그대로 유효합니다(필요하면 웹 UI의 사용자 화면에서 **토큰 모두 폐기**를 누르세요). 출력의 마지막 줄은 `done; sign in as <id> (a running server applies the change within a few seconds)`입니다.

### 보호

- **저장소 콘텐츠**: `/repository/` 응답에는 `Content-Security-Policy: sandbox allow-downloads; default-src 'none'`과 `X-Content-Type-Options: nosniff`가 붙어, 업로드된 HTML이 브라우저에서 이 서버의 권한으로 실행되지 않습니다. 인증된 요청의 응답은 `Cache-Control: private`입니다.
- bcrypt 비교는 동시에 최대 4개이며, 없는 사용자도 같은 비용으로 비교해 사용자 존재 여부를 응답 시간으로 알 수 없습니다. 성공한 비밀번호 확인은 1분간 캐시되고(비밀번호를 바꾸면 바로 무효), 토큰 마지막 사용 시각은 1분에 한 번만 기록합니다. 로그인 이름이 256바이트, 비밀번호가 1024바이트를 넘으면 어떤 Realm에도 보내지 않고 거부합니다.
- **로그인 시도 제한**: 비밀번호 확인(`/repository/`와 API의 Basic 인증, 웹 UI 로그인, `npm login`, 본인 비밀번호 변경의 현재 비밀번호)은 로컬과 LDAP 모두 로그인 이름마다 실패를 셉니다. 성공 없이 **5번** 실패한 이름은 마지막 실패로부터 **1분** 동안 비밀번호를 확인하지 않고 `429 Too Many Requests`로 거절됩니다(`Retry-After` 헤더에 남은 초, 웹 UI에는 기다릴 시간이 나옵니다). 그 뒤에도 실패하면 거절 시간이 실패마다 두 배가 되어 최대 **15분**까지 늘어납니다. 성공하거나 **1시간** 동안 실패가 없으면 잊습니다. 이름은 대소문자를 구분하지 않고 셉니다. 없는 이름도 같은 규칙이라 거절 여부로 계정이 있는지 알 수 없고, 디렉터리 장애로 실패한 LDAP 로그인도 실패로 셉니다. 같은 이름의 확인은 한 번에 하나씩 하므로 병렬 요청으로 한꺼번에 여러 비밀번호를 시험할 수 없고, 맞는 비밀번호를 쓰는 병렬 빌드는 첫 확인을 기다렸다가 캐시로 통과합니다.
  - 출력 가능한 ASCII(영문자, 숫자, 기호, 공백)가 아니거나 앞뒤에 공백이 있거나 공백이 연달아 있는 이름은 어떤 Realm에도 보내지 않고 틀린 비밀번호와 똑같이 401로 거부하며, 실패로 세지도 않습니다(웹 UI 로그인 창은 이름 앞뒤의 공백을 지우고 보냅니다). LDAP 디렉터리는 이름을 찾을 때 대소문자, 전각·반각, 유니코드 정규화 형태, 앞뒤·연속 공백의 차이를 무시하므로(RFC 4518), 그런 이름까지 받으면 한 계정을 철자만 바꿔 가며 제한 없이 시도할 수 있기 때문입니다. 로컬 사용자 ID는 원래 이 규칙에 맞으므로 영향이 없고, LDAP도 `uid`, `sAMAccountName`, ASCII `mail`이나 `Alice Smith`처럼 공백이 하나씩인 이름은 그대로 로그인합니다. 다만 `userFilter`가 한글 `cn`처럼 ASCII가 아닌 값으로 사용자를 찾는 설정에서는 그 값으로 로그인할 수 없으므로 `uid` 같은 ASCII 속성으로 찾게 하세요.
  - 한 클라이언트 주소(IPv6는 /64)가 첫 실패로부터 15분 안에 **20번** 실패하면, 그 15분이 끝날 때까지 그 주소에서는 이름마다 한 번만 실패해도 거절됩니다(여러 계정에 흔한 비밀번호를 대입하는 공격 대비). 주소 전체를 막지는 않습니다. 리버스 프록시 뒤에서는 모든 클라이언트가 프록시 주소로 보이므로(`X-Forwarded-For`는 보지 않습니다) 한 클라이언트가 모두를 막지 못하게 하기 위해서이며, 그 대신 프록시 뒤에서는 주소 기준 제한을 모든 클라이언트가 함께 받습니다.
  - 거절은 저절로 풀리고, 관리자가 그 사용자의 비밀번호를 재설정하거나 `user reset-password`를 실행해도 풀립니다. 기록은 메모리에만 있어(이름과 주소를 합쳐 최대 10000개) 재시작하면 지워집니다. 이름이나 주소가 한도에 닿으면 WARN 로그(`sign-in throttled`)가 한 번 남습니다. 한도는 고정값이며 설정으로 바꿀 수 없습니다.
  - API 토큰과 이미 로그인한 세션은 영향을 받지 않습니다. 누군가 일부러 계속 틀리면 그 이름의 비밀번호 로그인은 계속 거절되므로, 빌드와 CI는 비밀번호 대신 API 토큰을 쓰세요. 토큰 형식의 값(`cc_`나 `rl_` 뒤에 16진수 64자리)을 Basic 비밀번호로 보냈다가 토큰 확인에 실패한 요청(폐기했거나 잘못된 토큰)은 실패로 세지 않습니다.

### 메타데이터 데이터베이스

기본값은 `<dataDir>/meta.db`의 SQLite입니다(docker-compose, 소규모 설치). NFS처럼 파일 잠금이 느린 스토리지(Kubernetes의 NAS 볼륨 등)에서는 쓰기마다 몇 초씩 기다리게 되므로 PostgreSQL을 쓰십시오. 그러면 볼륨에는 blob만 남고, blob은 NFS에 두어도 됩니다.

```yaml
database:
  url: postgres://cc:비밀번호@db.example:5432/commandcenter?sslmode=require
  maxConns: 10        # 생략 시 SQLite 4, PostgreSQL 10
```

- 환경 변수 `COMMANDCENTER_DATABASE_URL`로도 지정할 수 있습니다(Kubernetes에서는 Secret으로 넣는 것을 권장. Helm 차트의 `database.existingSecret`). 둘 다 지정하면 시작을 거부합니다.
- `postgres://`와 `postgresql://`만 허용합니다. URL과 비밀번호는 로그나 오류 메시지에 남지 않습니다. 사용자 이름과 비밀번호의 특수문자는 퍼센트 인코딩하십시오.
- PostgreSQL 13 이상. 데이터베이스는 미리 만들어 두어야 하며(`CREATE DATABASE commandcenter`), 테이블은 첫 시작 때 만들어집니다. 스키마 버전은 `schema_version` 테이블에 기록되고, 더 새 버전이 쓴 데이터베이스는 SQLite와 같이 시작을 거부합니다.
- 모든 CLI 명령(`token create`, `user reset-password`, `gc`, `secret-key rotate`)이 같은 설정을 읽습니다.
- 백업은 `pg_dump`로 합니다. 시스템 정보 화면의 메타데이터 DB 크기는 PostgreSQL에서는 `pg_database_size` 값입니다.
- 쓰기는 한 번에 하나씩 처리합니다(SQLite와 같은 규칙). 인스턴스는 여전히 하나만 띄웁니다.
- 기존 `meta.db`를 PostgreSQL로 옮기는 도구는 아직 없습니다. 새 데이터베이스는 첫 시작 때 `config.yaml`의 저장소·사용자를 가져와 시작합니다.
- NUL이나 잘못된 UTF-8이 든 이름(옛 빌드가 저장한 Maven 경로)은 PostgreSQL에 저장할 수 없으므로 400으로 거부합니다.

### 저장된 비밀값 암호화

SMTP 비밀번호, LDAP bind 비밀번호, proxy 저장소의 `remoteAuth` 비밀번호와 Bearer 토큰은 원격 서버에 그대로 보내야 하므로 사용자 비밀번호나 토큰처럼 해시로 저장할 수 없습니다. **비밀값 키를 정하면** 이 값들을 AES-256-GCM으로 암호화해 `meta.db`에 저장하고(`enc:v1:`로 시작하는 값) 쓸 때 풉니다. 그러면 `meta.db`, 볼륨 복사본, 백업 파일이 새어도 키 없이는 비밀값을 알 수 없습니다. 키가 없으면 평문으로 저장하고, 시작할 때마다 WARN 로그(`no secret key is configured`)가 한 줄 남습니다.

- **키 만들기**: 32바이트 난수를 base64로 적은 값입니다. `head -c 32 /dev/urandom | base64`나 `openssl rand -base64 32`로 만들고, 셸이 없는 PC에서는 `docker run --rm busybox sh -c 'head -c 32 /dev/urandom | base64'`를 씁니다.
- **키 주기**: 환경변수 `COMMANDCENTER_SECRET_KEY`에 그 값을 넣거나, 그 값을 담은 파일을 `config.yaml`의 `secretKeyFile`로 가리킵니다(파일은 서버 UID 65532만 읽을 수 있게 두세요). 둘 다 주면 시작하지 않습니다. compose는 `docker-compose.yml`의 주석 처리된 `COMMANDCENTER_SECRET_KEY` 줄을 쓰면 됩니다. Kubernetes에서는 키를 Secret으로 만들고 차트의 `secretKey.existingSecret`에 그 이름을 적습니다("Helm 차트로 설치" 참고. 참조 매니페스트라면 주석 처리된 `COMMANDCENTER_SECRET_KEY`(Secret 참조) 항목). `gc`, `token create` 명령도 같은 키가 필요하고(`docker exec`는 컨테이너의 환경변수를 물려받습니다), `user reset-password`는 키 없이도 됩니다.
- **처음 켤 때**: 키를 주고 시작하면 평문으로 남아 있던 비밀값을 그 자리에서 암호화하고 INFO 로그(`encrypted the stored secrets that were plain text`)를 남깁니다. 그 뒤 저장하는 값은 바로 암호화됩니다. 키와 비밀값은 로그에 남지 않습니다.
- **`remote` URL에 넣은 인증 정보는 암호화하지 않습니다.** `https://user:token@host/…`처럼 URL에 넣은 사용자 이름·비밀번호나 `?token=…` 같은 쿼리는 키를 정해도 `meta.db`에 평문으로 남습니다. 사용자 이름과 비밀번호는 URL에서 빼고 `remoteAuth`의 `username`/`password`에 넣으세요(원격이 Bearer 토큰을 받으면 `bearerToken`). 키를 정했는데 이런 proxy가 있으면 시작할 때마다 그 저장소 이름을 적은 WARN 로그(`the remote URL of these proxies holds a user name, password or query…`)가 한 줄 남습니다. URL은 로그에 남지 않습니다. 쿼리로만 토큰을 받는 원격은 옮길 곳이 없으니 `meta.db`와 백업의 접근을 제한하세요.
- **키를 잃으면 비밀값을 되살릴 수 없습니다.** 키는 반드시 백업하되 `meta.db` 백업과 **다른 곳**에 두세요. 키 없이 시작하면 `it is encrypted, and no secret key is set`, 다른 키로 시작하면 `it was encrypted with another key` 오류로 시작하지 않습니다(다른 키로는 아무것도 다시 쓰지 않습니다). 키를 정말 잃었다면 서버를 멈추고, "백업과 복원"에서처럼 같은 UID로 `sqlite3 /data/meta.db`를 열어 암호화된 값을 지운 뒤 새 키로 시작하고 UI에서 비밀번호를 다시 입력합니다.

  ```sql
  UPDATE settings SET value = json_set(value, '$.password', '') WHERE key = 'smtp';
  UPDATE settings SET value = json_set(value, '$.bindPassword', '') WHERE key = 'ldap';
  UPDATE repositories SET config = json_remove(config, '$.remoteAuth.password', '$.remoteAuth.bearerToken') WHERE json_extract(config, '$.remoteAuth') IS NOT NULL;
  ```

- **키 교체**: `secret-key rotate`가 저장된 비밀값을 지금 키에서 새 키로 한 트랜잭션에 다시 암호화합니다. 실행 중인 서버는 시작할 때 읽은 키를 메모리에 계속 들고 있어서, 서버가 도는 동안 바꾸면 바뀐 값을 읽지 못하고 그 뒤 저장하는 값은 이전 키로 암호화해 두 키가 섞입니다. 그래서 **서버를 먼저 멈추고** 실행한 뒤 새 키로 시작합니다. 실행 전에 백업을 받아 두세요("백업과 복원" 참고).

  ```bash
  docker compose stop
  head -c 32 /dev/urandom | base64 > new.key   # 새 키. 컨테이너의 UID 65532가 읽을 수 있어야 합니다
  docker compose run --rm -v "${PWD}/new.key:/run/new.key:ro" commandcenter secret-key rotate -new-key-file /run/new.key
  # 끝나면 COMMANDCENTER_SECRET_KEY(.env 등)나 secretKeyFile이 가리키는 파일을 new.key의 값으로 바꿉니다
  docker compose up -d
  ```

  - 지금 키는 서버와 같이 `COMMANDCENTER_SECRET_KEY`나 `secretKeyFile`에서 읽으므로, 키 설정은 명령이 끝난 **뒤에** 바꿉니다. 아직 키를 정한 적이 없다면 지금 키는 없어도 됩니다. 새 키는 `-new-key-file`이 가리키는 파일에서 같은 형식(32바이트 난수의 base64, 위의 "키 만들기" 참고)으로 읽습니다. `-new-key-file`이 없거나, 그 파일을 읽지 못하거나, 형식이 틀리면 아무것도 바꾸지 않고 오류로 끝납니다.
  - 지금 키로 암호화된 값은 새 키로 다시 암호화하고, 평문으로 남아 있던 값도 새 키로 암호화합니다. 지금 키로 풀리지 않는 값이 하나라도 있으면(`it was encrypted with another key`, `it is encrypted, and no secret key is set`) 아무것도 바꾸지 않고 `stored secrets (nothing was changed): …` 오류와 0이 아닌 코드로 끝납니다. 교체를 마친 뒤 같은 명령을 다시 실행해도 이렇게 거부됩니다.
  - 출력은 개수뿐이며 키와 비밀값은 나오지 않습니다(예: `re-encrypted 3 stored secrets with the new key and encrypted 1 that were plain text`). 마지막 줄은 `done; start the server with the new key in COMMANDCENTER_SECRET_KEY or secretKeyFile`입니다.
  - `.env`의 값을 바꿨다면 `docker compose start`가 아니라 `docker compose up -d`로 시작하세요. `start`는 컨테이너를 만들 때의 환경변수를 그대로 씁니다. Git Bash에서는 "백업과 복원"처럼 `MSYS_NO_PATHCONV=1`을 앞에 붙이고 `"$(pwd -W)/new.key:/run/new.key:ro"`를 씁니다.
  - 교체 전에 받은 백업은 이전 키로만 풀립니다. 그 백업을 보관하는 동안은 이전 키도 따로 보관하세요. `new.key`는 키를 옮긴 뒤 지웁니다.
  - Kubernetes에서는 "서버를 멈춰야 하는 작업"의 일회용 Pod를 서버 이미지로 바꿔 실행하고(지금 키의 `COMMANDCENTER_SECRET_KEY` Secret 참조, 설정 Secret과 새 키 파일의 마운트를 함께 둡니다), 키 Secret(차트의 `secretKey.existingSecret`)을 새 키로 바꾼 뒤 `--replicas=1`로 되돌립니다.
- 키를 켠 뒤 이 기능이 없는 이전 버전으로 내리면, 이전 버전은 암호화된 값을 그대로 비밀번호로 보내 원격·SMTP·LDAP 인증이 실패합니다("업그레이드" 참고).
- 키가 없을 때는 `enc:v1:`로 시작하는 비밀번호를 저장하지 않습니다(암호화된 값과 구별할 수 없기 때문입니다).

## Realm과 LDAP

**Realm**은 인증 방식입니다. Realm 화면에서 순서와 켜기/끄기를 정합니다(`GET/PUT /-/api/realms`).

| Realm | 하는 일 |
|---|---|
| `local` | DB에 저장된 로컬 사용자의 비밀번호 로그인(UI와 Basic 인증) |
| `token` | API 토큰 |
| `ldap` | LDAP 디렉터리의 사용자·비밀번호 로그인 |

비밀번호 로그인은 켜진 Realm을 **목록 순서대로** 시도합니다(기본은 `local`, `token`이며 `ldap`은 설정을 마친 뒤 켭니다). 앞 Realm에서 실패하면 다음으로 넘어가므로 같은 이름이 로컬과 LDAP에 모두 있으면 순서가 우선순위입니다. `token` Realm을 끄면 모든 토큰이 거부됩니다. `ldap`은 URL, 사용자 기준 DN, 필터를 채우기 전에는 켤 수 없고, `local`이나 `ldap`을 끄는 변경에는 위의 잠금 방지가 적용됩니다.

`local`과 `ldap` Realm은 **비밀번호 로그인만** 막습니다. `local` Realm을 꺼도 로컬 사용자의 이미 로그인한 웹 세션과 API 토큰(토큰은 `token` Realm이 판단)은 계속 동작합니다. 끊어야 하면 그 사용자를 비활성화하거나 토큰을 폐기하세요.

### LDAP 설정 필드

| 필드 | 기본값 | 설명 |
|---|---|---|
| `url` | 없음 | `ldap://호스트[:포트]` 또는 `ldaps://호스트[:포트]` |
| `startTLS` | `false` | `ldap://`에서 StartTLS로 암호화(`ldaps://`와는 같이 쓸 수 없음) |
| `insecureSkipVerify` | `false` | 서버 인증서 검증 생략. 시험용으로만 쓰세요. 사내 CA가 발급한 인증서라면 검증을 끄지 말고, 그 CA 인증서(PEM 파일)를 컨테이너의 한 디렉터리에 마운트한 뒤 환경변수 `SSL_CERT_DIR`로 그 디렉터리를 알려 주세요(예: `SSL_CERT_DIR=/etc/ssl/internal-ca`). 이 설정은 SMTP와 proxy의 원격 연결에도 적용되며, 기본 인증서 묶음도 계속 읽으므로 공개 CA를 쓰는 연결에는 영향이 없습니다. |
| `bindDN`, `bindPassword` | 없음 | 검색에 쓰는 서비스 계정. `bindDN`이 없으면 익명 검색. 비밀번호는 저장만 되고(비밀값 키를 정하면 암호화해서, "저장된 비밀값 암호화" 참고) API 응답·로그·이벤트에 나오지 않습니다(`bindPasswordSet`만 표시). 비워서 보내면 기존 값을 유지하고 `clearBindPassword: true`로 지웁니다. 단 **서버의 호스트:포트를 바꾸거나, `ldaps`에서 `ldap`으로 바꾸거나, `startTLS`를 끄거나, `insecureSkipVerify`를 켤 때는 비밀번호를 다시 입력해야 합니다**(400). 저장된 비밀번호가 다른 서버나 더 약한 연결로 새어 나가지 않게 하는 규칙입니다. |
| `timeoutSeconds` | `5` | 연결·응답 제한(1..60) |
| `userBaseDN` | 필수 | 사용자를 검색할 기준 DN |
| `userFilter` | `(uid={user})` | 검색 필터. `{user}`가 필수이며 로그인 이름은 필터에 넣기 전에 이스케이프합니다. 정확히 한 항목이 나와야 합니다. |
| `userIdAttr` | `uid` | 사용자 ID로 쓸 속성 |
| `nameAttr`, `emailAttr` | `cn`, `mail` | 표시 이름, 이메일 |
| `groupAttr` | `memberOf` | 사용자의 그룹을 담은 속성 |
| `groupMappings` | 없음 | `[{group, roles}]` 그룹 → 역할 |
| `defaultRoles` | 없음 | 모든 LDAP 사용자에게 주는 역할 |

### 로그인 흐름

1. 서비스 계정으로 바인드(`bindDN`이 있을 때)하고 `userBaseDN` 아래에서 `userFilter`로 사용자를 한 명 찾습니다(0명이나 2명 이상이면 실패).
2. 찾은 항목의 DN으로 사용자 비밀번호를 바인드합니다. 이름이나 비밀번호가 비어 있으면 서버에 보내지 않고 거부합니다(익명 바인드 방지). 출력 가능한 ASCII가 아니거나 앞뒤·연속 공백이 있는 이름도 디렉터리에 보내지 않습니다("보호"의 로그인 시도 제한 참고).
3. 사용자의 `groupAttr` 값을 그룹 매핑에 대응시켜 역할을 정합니다. 성공한 비밀번호 확인은 로컬과 같이 1분간 캐시합니다.

LDAP 사용자는 DB에 사용자 행이 따로 없고, 로그인하면 메모리에 항목이 생깁니다. 세션과 토큰의 소유자 키는 `ldap:<id>`이며(예: `ldap:alice`) 같은 ID의 로컬 사용자와 섞이지 않습니다. `/-/api/me`는 `user`에 `alice`를, `owner`에 `ldap:alice`를 돌려주고, API 토큰 화면은 `owner`로 본인 토큰을 찾습니다. LDAP 사용자의 계정 화면에도 이 소유자 키가 나옵니다. 사용자 목록에서는 같은 ID의 로컬 사용자와 LDAP 사용자가 Realm이 다른 두 행으로 따로 나옵니다. LDAP 사용자는 같은 ID의 역할 덮어쓰기 행(아래)이 있을 때만 그 행과 한 행으로 합쳐지고, 그 행에는 그룹 매핑이 준 역할까지 더한 역할이 나옵니다. 토큰을 Basic 인증의 비밀번호로 쓸 때 사용자 이름은 `ldap:`을 뺀 `alice`입니다. 역할은 요청마다 최신 설정으로 계산하므로 매핑을 바꾸면 곧바로 반영됩니다.

### 그룹 매핑

`groupMappings[].group`은 사용자의 그룹 속성 값과 **전체 DN**(`cn=cc-admins,ou=groups,dc=example,dc=org`) 또는 **첫 RDN의 값인 CN**(`cc-admins`)으로, 대소문자 구분 없이 비교합니다. 일치한 매핑의 역할과 `defaultRoles`의 합집합이 사용자 역할입니다. 주의하세요.

- **CN만 적은 매핑은 같은 CN의 다른 그룹과도 일치합니다.** 서로 다른 OU에 같은 이름의 그룹이 있을 수 있으면 전체 DN으로 적으세요. 특히 `admin` 역할을 주는 매핑은 DN으로 적는 것이 안전합니다.
- `defaultRoles`에 `admin`을 넣으면 디렉터리의 모든 사용자가 관리자가 됩니다(UI가 경고합니다).
- `memberOf` 같은 속성을 서버가 채워 줘야 합니다(OpenLDAP은 memberOf overlay가 필요). 중첩 그룹은 풀지 않습니다.
- Active Directory에서 비활성화된 계정은 자동으로 막히지 않습니다. 필터가 지원하는 `&`, `|`, `!`, 등호, 존재(`attr=*`) 조건으로 허용 그룹을 `userFilter`에 넣으세요(예: `(&(sAMAccountName={user})(memberOf=CN=허용그룹,…))`). AD 확장 매칭 규칙(`:1.2.840…:=`), 부분 일치, 비교 필터는 지원하지 않습니다.

### 로컬 역할 덮어쓰기

사용자 화면에서 `source`가 `ldap`인 사용자를 같은 ID로 만들면 **역할 덮어쓰기 행**이 됩니다. 이 행은 그룹 매핑이 준 역할에 자신의 역할을 **더하고**, `disabled`로 두면 그 LDAP 사용자의 로그인을 막습니다. 행을 지우면 그 사용자가 다시 로그인할 수 있으므로 `disabled`인 행은 지울 수 없습니다(409, `this disabled record blocks LDAP user <id>`). 다시 허용하려는 것이 맞다면 먼저 `active`로 바꾼 뒤 지우세요. 이 행은 로그인 주체가 아니며(비밀번호가 없고, 세션·토큰도 이 행으로는 생기지 않음) 디렉터리에서 사용자가 사라져도 남습니다.

### 1분 재확인과 장애 시 동작

- LDAP 사용자의 항목은 1분이 지나면 다음 사용 때 서비스 계정으로 다시 검색해 확인합니다(**세션, 토큰, 비밀번호 캐시 모두**). 디렉터리에서 사라졌거나 검색 결과가 모호하면 즉시 거부합니다.
- 디렉터리에 닿을 수 없으면 **닫힌 채로(fail closed)** 거부합니다. 재확인이 실패하면 그 시도가 끝난 때부터 10초 동안은 다시 시도하지 않고 거부합니다(클라이언트가 먼저 연결을 끊은 경우는 실패로 치지 않습니다). 한 사용자의 재확인은 한 번에 하나만 하고 같은 사용자의 다른 요청은 그 결과를 기다리며, 기다리는 요청은 클라이언트가 끊으면 바로 끝납니다. 재시작 직후에도 LDAP 사용자의 세션과 토큰은 한 번 재확인을 통과해야 합니다. 로컬 관리자는 영향을 받지 않습니다.
- 재확인은 `userFilter`의 `{user}`가 `userIdAttr`와 같은 값을 가리킨다고 가정합니다(예: `(uid={user})`와 `uid`). 다르면 재확인이 실패해 1분 뒤 세션·토큰이 거부됩니다.
- 로그인 경로의 **장애 백오프**: 연결·TLS 단계(TCP, TLS/StartTLS, 서비스 바인드의 네트워크 오류)가 연달아 3번 실패하면 15초 동안 로그인에서 `ldap` Realm을 건너뛰어(뒤의 Realm이 판단) 로컬 로그인이 느려지지 않게 합니다. 잘못된 비밀번호, 검색 실패, 너무 큰 응답 같은 요청별 실패는 장애로 세지 않습니다. 디렉터리 응답 메시지는 1 MiB로 제한합니다. 검색 결과 항목 하나에는 속성 64개, 속성 하나에는 값 1024개까지 받으며, 넘는 사용자(예: 직접 속한 그룹이 1024개를 넘는 사용자)는 로그인할 수 없습니다.
- 디렉터리에서 사용자가 사라져도 토큰과 세션을 지우지는 않고 접근만 거부합니다(기준 DN 오타로 전부 폐기되는 일을 막기 위해서입니다). 같은 ID가 다시 생기면 이전 토큰이 되살아날 수 있으므로 재사용되지 않는 값을 `userIdAttr`로 쓰세요.

### 연결 테스트

`POST /-/api/realms/ldap/test`(`{user, password}`, 저장된 설정을 사용)는 `connect`, `starttls`, `bind`, `search`, `userBind`, `groups` 단계를 하나씩 실행해 `{ok, steps:[{step, ok, detail}]}`로 돌려줍니다. 사용자 이름을 주면 `search`까지, 비밀번호도 주면 `userBind`와 `groups`(매핑에 일치한 그룹)까지 갑니다. 사용자 이름이 로그인 규칙(출력 가능한 ASCII, 앞뒤 공백과 연속 공백 없음)에 맞지 않으면 맨 앞에 `name` 단계가 실패로 붙고 `ok`는 `false`가 됩니다. 디렉터리가 그 이름으로 항목을 찾더라도 로그인은 그 이름을 거부하기 때문입니다. API는 이름을 보낸 그대로 검사합니다(설정 화면은 앞뒤 공백을 지우고 보냅니다). 응답에 비밀번호는 나오지 않습니다. 설정 화면의 테스트 버튼이 이 호출입니다.

### 예: bitnami/openldap 스타일

```
url:           ldap://openldap:1389
bindDN:        cn=admin,dc=example,dc=org
bindPassword:  (서비스 계정 비밀번호)
userBaseDN:    ou=people,dc=example,dc=org
userFilter:    (&(objectClass=inetOrgPerson)(uid={user}))
userIdAttr:    uid          groupAttr: memberOf
groupMappings: [{ "group": "cc-admins", "roles": ["admin"] }]
```

`test/smoke/ldap-smoke.sh`가 이 구성을 컨테이너로 띄워 로그인, 토큰, 잠금 방지를 확인합니다.

## Blob Store

blob(파일 내용)은 **blob store** 디렉터리에 sha256 이름으로 저장됩니다.

- **default store**: `<dataDir>/blobs`. 기존 설치와 같은 위치이며 이름은 `default`입니다. 삭제할 수 없습니다. 현재 `dataDir`를 기준으로 하므로 `dataDir`를 옮겨도 따라갑니다.
- **이름 붙은 store**: Blob Store 화면(`POST /-/api/blobstores`)에서 만듭니다. 경로를 비우면 `<dataDir>/blobstores/<이름>`이고, 절대 경로를 주면 그곳을 씁니다. 상대 경로는 400입니다(이전 버전은 상대 경로를 말없이 무시하고 기본 위치에 만들었습니다). 경로가 데이터 디렉터리 밖이면 응답과 `GET /-/api/blobstores`의 `warning`, Blob Store 상세 화면에 경고가 나옵니다. 그런 경로는 영구 볼륨으로 따로 마운트하고 따로 백업해야 합니다("저장 공간과 인스턴스", "백업과 복원" 참고). `<경로>/blobs/sha256`이 **없거나 비어 있어야** 하고(이미 blob이 있으면 거부, 다른 파일이 있는 디렉터리는 괜찮습니다), 쓸 수 있어야 하며, 다른 store의 blob 디렉터리와 겹치거나 심볼릭 링크로 같은 곳을 가리키면 거부됩니다. 기존 store의 경로를 확인할 수 없으면(끊어진 심볼릭 링크 등) 겹치지 않는다고 확신할 수 없으므로 그 store 이름을 알려 주며 거부합니다. 이름 규칙은 저장소와 같습니다.
- **저장소는 만들 때 store를 고릅니다**(`blobStore`, 생략하면 `default`). 저장소를 만든 뒤에는 **바꿀 수 없습니다.** group에는 store가 없습니다.
- store마다 **중복 제거와 GC가 독립**입니다. 같은 파일을 서로 다른 store의 저장소에 올리면 각각 저장되고, GC는 자기 store의 저장소가 참조하지 않는 blob만 지웁니다. 단 **default store의 GC는 어느 저장소든 참조하는 blob은 지우지 않습니다.** 이름 붙은 store를 모르는 이전 버전으로 돌린 적이 있으면 그 저장소들의 파일이 default store에 쓰였을 수 있고, 그것이 유일한 사본일 수 있기 때문입니다("업그레이드"의 다운그레이드 안내 참고). 그래서 같은 파일이 default store의 저장소에서 지워진 뒤에도, 이름 붙은 store의 저장소가 참조하는 동안은 default store의 사본이 남습니다. 존재하지 않는 store를 가리키는 저장소가 있으면(설정 가져오기 등) 조용히 default로 가지 않고 오류로 응답하며, GC는 그 저장소가 모든 store를 참조 중인 것으로 보고 지우지 않습니다.
- **사용량**: Blob Store 화면과 `GET /-/api/blobstores`가 store별 blob 수·용량(그 store의 저장소가 참조하는 것), 저장소 수, 파일시스템 전체/남은 용량(리눅스)을 보여 줍니다.
- **삭제**: 저장소가 쓰고 있는 store는 409입니다. 삭제해도 **디렉터리와 파일은 디스크에 남습니다**(관리자가 직접 정리하세요).
- **compact 작업**(`blobstore.compact`)은 선택 파라미터 `blobStore`로 한 store만 정리하고, 생략하면 모든 store를 정리합니다("예약 작업" 참고).

## 일반 설정

일반 설정 화면(`GET/PUT /-/api/settings`, 관리자)의 값은 저장 즉시 적용됩니다. 바꿀 항목만 보내면 되고, 보낸 키만 저장합니다.

| 키 | 기본값 | 설명 |
|---|---|---|
| `baseURL` | 가져온 값 또는 `http://localhost:8081` | 클라이언트에게 알려 주는 주소(npm·Cargo 응답의 URL, PyPI 파일 링크의 경로 앞부분, 세션 쿠키의 이름과 `Secure` 여부, 교차 출처 쓰기 검사에서 허용하는 출처). 바로 반영됩니다. 사용자 정보(`user:pass@`), 쿼리, 프래그먼트가 있으면 400입니다. 이전 버전이 저장한 그런 값은 바꿀 때까지 그대로 쓰이고, 서버가 시작할 때마다 WARN 로그(`the stored baseURL is no longer accepted`, 값은 기록하지 않음)가 남습니다. |
| `anonymousRead` | `false` | 인증 없는 읽기(저장소별 값이 우선) |
| `authenticatedRead` | `true` | 로그인한 사용자의 읽기·browse를 권한 없이 허용 |
| `sessionIdleMinutes` | `720` | 세션 유휴 만료(5..10080분). 이미 로그인한 세션에도 적용됩니다. |
| `maxUploadMiB` | `1024` | 업로드 본문 최대 크기(1..1048576). 요청마다 읽으므로 바로 적용됩니다. |
| `defaultLanguage` | `ko` | `ko` 또는 `en`. 브라우저에 저장된 언어 선택이 없을 때 웹 UI가 쓰는 언어이고(언어 버튼으로 고르면 그 선택이 저장되어 우선합니다), 작업 실패 알림 메일의 언어입니다. `GET /-/api/me`가 로그인하지 않은 사용자에게도 알려 줍니다. |

## 이메일 알림

SMTP 설정(`GET/PUT /-/api/settings/smtp`, 관리자)으로 예약 작업 실패를 메일로 알립니다.

- **필드**: `enabled`, `host`, `port`, `security`, `username`, `password`, `from`. 켜려면 host, port, 올바른 `from`이 필요합니다. 끄면 일부만 채워도 저장됩니다. host에 공백이나 제어 문자(CR, LF 등)가 있으면 꺼져 있어도 400입니다. PUT은 전체 교체이므로 폼의 모든 값을 보내야 하고, 생략한 `password`만 기존 값을 유지합니다.
- **보안 모드**: `none`(암호화 없음), `starttls`(연결 뒤 업그레이드, 서버가 STARTTLS를 광고해야 함), `tls`(처음부터 TLS, 보통 465). TLS 인증서는 항상 검증하며 건너뛰는 옵션은 없습니다. 인증은 `username`을 채웠을 때만 **PLAIN**(`PlainAuth`)이며 LOGIN, CRAM-MD5 등은 지원하지 않습니다. Go의 PlainAuth는 암호화하지 않은 연결에서 localhost가 아닌 서버로는 비밀번호를 보내지 않으므로, 인증이 필요하면 `starttls` 또는 `tls`를 쓰세요.
- **헤더**: 메일마다 `Date`와 서버가 만든 `Message-ID`(`<임의 문자열@from 주소의 도메인>`)를 붙입니다. `Message-ID`가 없으면 수신 서버가 대신 붙이는데, 그러면 스팸 점수가 오를 수 있습니다.
- **비밀번호**: 저장만 되고(비밀값 키를 정하면 암호화해서, "저장된 비밀값 암호화" 참고) 응답·로그·이벤트에 나오지 않습니다(`passwordSet`만 표시). 비워서 보내면 기존 값을 유지하고 `clearPassword: true`로 지웁니다. **서버(host나 port)를 바꾸거나 `tls`/`starttls`에서 `none`으로 낮출 때는 비밀번호를 다시 입력해야 합니다**(400). 저장된 비밀번호가 다른 서버나 암호화되지 않은 연결로 새어 나가지 않게 하는 규칙입니다.
- **테스트 메일**: `POST /-/api/settings/smtp/test`(`{to}`)는 저장된 설정으로 한 통을 바로 보내고 성공하면 200입니다. 이메일이 꺼져 있으면 400, **10초 안에 다시 호출하면 429**, 발송 실패는 502입니다. 10초 창은 이메일이 켜져 있는지 확인한 뒤 **보내기 전에** 잡으므로 실패한 발송도 한 번으로 셉니다.
- **작업 실패 알림**: 작업의 `notifyEmail`(최대 20개 주소)에 수신자를 넣으면, 그 작업의 실행이 **Failed**로 끝날 때만(성공, 중지, 서버 종료 중 실패는 제외) 이메일이 켜져 있을 때 메일을 보냅니다. 제목에 작업 이름이 들어가고 본문에는 시각, 결과 메시지, **마지막 로그 20줄**이 담깁니다. 언어는 `defaultLanguage`를 따릅니다. 메일은 백그라운드로 보내므로 작업을 지연시키지 않고, 큐(256건)가 차면 버립니다. 발송 실패는 서버 로그(WARN)에 남고 활동 기록에 `email.failed` 이벤트(제목과 오류 메시지)로도 기록됩니다.

## 저장소 Online/Offline

저장소 편집 화면(또는 API의 `online`)에서 저장소를 **Offline**으로 바꿀 수 있습니다(기본은 Online).

- Offline 저장소의 `/repository/<이름>/…` 요청은 **503**(`repository is offline`)입니다. 권한 검사를 먼저 하므로 읽을 수 없는 사용자는 Offline인지 알 수 없고 401/403을 받습니다.
- **Maven·Go·Cargo group은 Offline 멤버를 건너뜁니다. npm·PyPI group은 건너뛰지 않고 실패합니다**: 요청한 패키지를 가진 멤버보다 앞에 Offline 멤버가 있으면(어느 멤버도 그 패키지를 갖지 않을 때도) 503 `group member "<이름>" is offline`으로 답합니다. 점검하려고 hosted를 잠시 Offline으로 돌렸을 때 proxy에 있는 같은 이름의 공개 패키지가 대신 설치되지 않게 하기 위해서입니다. 패키지를 가진 멤버보다 뒤에 있는 멤버는 조회하지 않으므로 Offline이어도 영향이 없습니다. `writeMember`가 Offline이면 그 group에 쓰는 요청은 (쓰기 권한이 있는 사용자에게) 503 `repository is offline`이고, UI 저장소 목록의 `canWrite`도 `false`입니다. group 자체를 Offline으로 두면 그 group이 503입니다.
- group의 멤버가 **모두** Offline이면(멤버인 group의 멤버가 모두 Offline인 경우 포함) 그 group을 읽는 요청도 503 `repository is offline`입니다. 404를 돌려주면 Maven 같은 클라이언트가 not-found를 캐시해 멤버를 Online으로 되돌린 뒤에도 바로 받지 못하기 때문입니다. 쓰기는 위의 `writeMember` 규칙을 그대로 따르고, npm `login`처럼 권한이 필요 없는 경로는 영향을 받지 않습니다(읽기 권한이 필요한 `npm ping`도 503입니다). 멤버가 하나도 없는 group은 Offline으로 보지 않습니다.
- **cleanup 정책과 compact 작업은 영향을 받지 않고** Offline 저장소에서도 실행됩니다.

## 포맷별 클라이언트 설정

### Maven 클라이언트 설정

`~/.m2/settings.xml`:

```xml
<settings>
  <servers>
    <server><id>commandcenter</id><username>ci</username><password>비밀번호</password></server>
  </servers>
  <mirrors>
    <mirror><id>commandcenter</id><mirrorOf>*</mirrorOf>
      <url>http://localhost:8081/repository/maven-public/</url></mirror>
  </mirrors>
  <profiles>
    <profile>
      <id>commandcenter</id>
      <repositories>
        <repository><id>central</id><url>http://central</url>
          <releases><enabled>true</enabled></releases>
          <snapshots><enabled>true</enabled></snapshots></repository>
      </repositories>
      <pluginRepositories>
        <pluginRepository><id>central</id><url>http://central</url>
          <releases><enabled>true</enabled></releases>
          <snapshots><enabled>true</enabled></snapshots></pluginRepository>
      </pluginRepositories>
    </profile>
  </profiles>
  <activeProfiles>
    <activeProfile>commandcenter</activeProfile>
  </activeProfiles>
</settings>
```

`<mirror>`만 있으면 Maven 기본 저장소 `central`의 SNAPSHOT 꺼짐 정책이 그대로 적용되어 SNAPSHOT 의존성을 받지 못합니다(BUILD FAILURE). profile이 `central`을 SNAPSHOT을 켠 채로 다시 선언하고, 그 요청은 mirror를 거쳐 이 서버로 옵니다(`http://central`에는 실제로 접속하지 않습니다). 웹 UI의 proxy·group 저장소 Repository setup도 같은 설정을 보여 줍니다.

배포 대상은 `pom.xml`의 `distributionManagement`에 `http://localhost:8081/repository/maven-releases/`(id `commandcenter`)로 지정합니다.
다른 컨테이너에서 접근할 때는 `localhost` 대신 `host.docker.internal`을 사용하세요.

Maven 동작 메모:

- SNAPSHOT 버전 디렉터리의 `maven-metadata.xml`은 서버가 만듭니다. 그 디렉터리의 파일을 올리거나 지울 때마다, 그리고 `repository.rebuild-metadata` 작업이 돌 때 가장 높은 timestamp 빌드를 기준으로 다시 만듭니다. 클라이언트(`mvn deploy`)가 올린 같은 문서는 201로 답하되 무시합니다.
- GA 디렉터리(`groupId/artifactId/`)의 `maven-metadata.xml`도 서버가 저장된 아티팩트로 만듭니다. 버전 목록, `latest`, `release`, `groupId`, `artifactId`는 아티팩트를 올리거나 지울 때마다 다시 계산합니다. 클라이언트(`mvn deploy`)가 올린 문서는 201로 답하되 플러그인 그룹 문서의 `<plugins>`만 저장된 문서에 합치고, 나머지(버전, `<latest>`, `<release>`, 경로와 다른 groupId/artifactId)는 무시합니다. 그래서 배포 권한만으로는 아티팩트가 없는 버전을 끼워 넣어 `[1.0,)` 같은 버전 범위 해석을 바꿀 수 없습니다. 다만 groupId/artifactId가 경로의 GA를 가리키는 문서(`mvn deploy`가 아티팩트를 다 올린 뒤 올리는 문서)를 받으면 그 GA의 문서를 저장된 아티팩트로 다시 만듭니다. 그래서 아티팩트 업로드 뒤의 갱신이 실패했더라도(아티팩트 업로드는 201, 서버 로그에 `maven-metadata.xml rebuild failed`) 배포의 마지막 단계에서 바로잡힙니다. 이때 다시 만들기가 실패하면 그 메타데이터 업로드가 5xx로 실패합니다. GA 디렉터리가 플러그인 그룹의 groupId 디렉터리이기도 하면(groupId `org.example`의 플러그인 목록과 GA `org:example`이 함께 쓰는 `org/example/maven-metadata.xml`) 아티팩트나 컴포넌트를 지울 때(Cleanup 포함)와 `repository.rebuild-metadata` 작업이 문서를 다시 만들 때도 `<plugins>`는 그대로 남고, 그 GA의 마지막 버전을 지우면 `<plugins>`만 남습니다. 이전 버전에서 클라이언트 문서로 들어간 버전은 그 GA에 아티팩트나 GA 문서를 하나 더 올리거나(다음 `mvn deploy`) `repository.rebuild-metadata` 작업을 돌리면 사라집니다(업그레이드 후 한 번 돌리기를 권장합니다). 아티팩트가 하나도 없는 GA의 문서는 이 작업이 버전을 지웁니다(`<plugins>`가 있으면 그것만 남기고, 없으면 문서를 지웁니다). 문서에 적힌 `groupId`/`artifactId`가 없거나 다른 GA를 가리켜도, 문서가 다른 GA의 디렉터리 안(GA `org:example` 아래의 `org/example/app/`)에 있어도 마찬가지입니다.
- 올리는 `maven-metadata.xml`은 1 MiB까지 받습니다(넘으면 413). 업로드한 메타데이터는 동시에 2개까지만 처리하며, 슬롯이 모두 사용 중이면 30초까지 기다리다가 `503 too many concurrent maven-metadata.xml uploads, retry`로 거절하므로 잠시 후 다시 시도하세요. 슬롯을 잡은 업로드는 본문을 10초 안에 다 보내야 하며(계속 조금씩 보내도 마찬가지입니다), 넘기면 `408 maven-metadata.xml not received within 10s`로 답합니다. 그래서 느린 클라이언트 둘이 슬롯을 모두 잡아도 다른 업로드는 10초 남짓만 기다립니다. 본문은 슬롯을 잡은 뒤에 읽으므로, 슬롯을 기다리는 업로드는 본문을 메모리에 올리지 않습니다.
- 올리는 `.pom`(classifier가 없는 것)은 저장하기 전에 내용을 검사합니다. 1 MiB까지 받고(넘으면 413), 올바른 XML이면서 루트가 `<project>` 하나여야 하며, `groupId`(없으면 `<parent>`의 값), `artifactId`, `version`(없으면 `<parent>`의 값)이 업로드 경로의 좌표와 같아야 합니다. 그렇지 않으면 400입니다(예: `400 .pom declares "evil:other:9", want "org.example:pv:1.1"`). 거부된 업로드는 아무것도 남기지 않으므로 `allow_once` 저장소에서도 고쳐서 다시 올릴 수 있습니다. SNAPSHOT의 timestamp 파일은 디렉터리의 버전(`1.0-SNAPSHOT`)과 비교합니다. `groupId`와 `artifactId` 안의 `${이름}`은 그 POM의 `<properties>`로 바꾼 뒤 비교합니다. 거기에 없는 속성(부모 POM이 정하는 `${scala.binary.version}` 등)은 `/`와 `:`가 아닌 한 글자 이상과 맞는 것으로 보고, 나머지 부분은 그대로 비교합니다. 그래서 `<artifactId>hudi-spark_${scala.binary.version}</artifactId>`인 POM은 `hudi-spark_2.12` 경로에는 올라가지만 `other_2.12` 경로에는 400입니다. `version`에 `${…}`가 들어 있으면 비교하지 않습니다. 명령줄(`-Drevision=…`)이나 부모 POM이 정하는 CI-friendly 버전(`${revision}`)은 서버가 알 수 없기 때문이며, 그래서 flatten-maven-plugin 없이 `mvn -Drevision=1.2.3 deploy`로 배포해도 받습니다. `encoding="ISO-8859-1"`, `EUC-KR`처럼 다른 인코딩을 선언한 POM도 받습니다. 선언이 없거나 UTF-8로 선언한 POM에 UTF-8이 아닌 바이트가 있으면(예: MS949로 저장한 `<description>`) Maven처럼 그 바이트를 U+FFFD로 바꿔 읽으므로 받으며, 좌표는 그대로 비교합니다. classifier가 붙은 `.pom`과 이미 저장된 POM은 검사하지 않습니다.
- sbt 플러그인은 hosted 저장소에 `sbt publish`로 배포할 수 있습니다. sbt 1.9 이상은 기본 설정(`sbtPluginPublishLegacyMavenStyle := true`)에서 한 버전 디렉터리에 두 방식으로 올립니다. Maven 방식 파일은 이름에 디렉터리와 같은 `_<Scala 버전>_<sbt 버전>`이 붙고(`com/example/sbt-foo_2.12_1.0/1.0.0/sbt-foo_2.12_1.0-1.0.0.jar`), 옛 방식(legacy layout) 파일은 붙지 않습니다(`com/example/sbt-foo_2.12_1.0/1.0.0/sbt-foo-1.0.0.jar`). sbt 1.9 미만은 옛 방식으로만 올립니다. 서버는 두 방식을 모두 저장하고 hosted와 group에서 내줍니다. 옛 방식 파일은 디렉터리 이름이 `_<숫자>_<숫자>`(예: `_2.12_1.0`)로 끝나고 파일 이름이 그 접미사를 뺀 이름, `-`, 버전, classifier(있으면), 확장자로 되어 있을 때만 받고, 그 밖의 이름은 400입니다. 옛 방식 `.pom`은 접미사를 뺀 artifactId(`sbt-foo`)와 비교해 검사합니다(예: `400 .pom declares "com.example:sbt-foo_2.12_1.0:1.0.0", want "com.example:sbt-foo:1.0.0"`). 옛 방식 파일은 GA의 버전으로 치지 않으므로 `maven-metadata.xml`의 버전 목록에는 Maven 방식 파일이 있는 버전만 들어가고, sbt 1.9 미만으로 배포한 버전은 목록에 없습니다(버전을 정확히 적으면 받을 수 있습니다). 컴포넌트 검색, Cleanup, 컴포넌트 삭제(웹 UI의 "Delete component"와 API)에서는 두 방식의 파일(proxy에 캐시된 옛 방식 파일 포함)이 한 컴포넌트(`com.example:sbt-foo_2.12_1.0:1.0.0`)이므로 컴포넌트를 지우면 둘 다 지웁니다. Maven 방식 파일만 지우면 sbt가 옛 방식 파일로 그 버전을 계속 받기 때문입니다.
- proxy와 group은 Maven 파일만 조회하고, proxy는 그것만 원격에서 받아 캐시합니다: 아티팩트(서명 `.asc` 포함), `maven-metadata.xml`, `.meta/prefixes.txt`, `archetype-catalog.xml`, sbt의 옛 방식(legacy layout)으로 된 sbt 플러그인 파일과 그 체크섬입니다. 옛 방식 파일은 디렉터리 이름에만 `_<Scala 버전>_<sbt 버전>`이 붙고 파일 이름에는 붙지 않습니다(예: `com/eed3si9n/sbt-assembly_2.12_1.0/2.1.1/sbt-assembly-2.1.1.jar`). sbt와 coursier가 이 경로로 요청하므로 proxy와 group에서 받을 수 있고, hosted 저장소에도 올릴 수 있습니다(위 항목 참고). 디렉터리(`junit/junit/4.13.2`, `junit/junit/`)처럼 그 밖의 경로는 원격에 묻지 않고 404로 답하므로, proxy 주소를 브라우저로 열어 원격의 디렉터리 목록을 볼 수는 없습니다(캐시된 파일은 웹 UI의 Browse에서 봅니다). IDE가 받는 원격 인덱스(`.index/`)도 받지 않습니다. 이름이 `.html`로 끝나지 않는 파일에 원격이 `text/html`로 답하면(디렉터리 목록이나 오류 페이지) 저장하지 않습니다. 캐시에 없던 파일이면 404로 처리하며 negative cache에 기록합니다. 이때 서버 로그에 `remote answered with a web page, taken as not found`가 남으니, 원격 서버가 아티팩트를 `text/html`로 잘못 내주는지 이 줄로 확인하세요. 이미 캐시된 파일(만료된 `maven-metadata.xml` 등)을 다시 확인할 때 이런 답이 오면 원격 장애(점검 페이지나 로그인 페이지)로 보고, 원격에 접속할 수 없을 때처럼 캐시된 사본을 그대로 줍니다. 이때는 서버 로그에 `remote fetch failed`와 `remote answered with a web page`가 남습니다. 이전 버전이 캐시한 디렉터리 목록과 IDE 인덱스(`.index/`)는 이제 제공하지 않지만(404) Browse에는 남아 있습니다. 해는 없으며, 지우려면 그 proxy를 대상으로 `repository.rebuild-metadata` 작업을 한 번 실행하세요(업그레이드 후 proxy마다 한 번 돌리기를 권장합니다. "예약 작업" 참고). 이 작업은 proxy가 더는 받지 않는 경로(Maven 파일이 아닌 경로)의 캐시만 지우고, Maven 파일 경로의 캐시는 `text/html`이어도 남깁니다. 원격이 형식을 잘못 표시한 진짜 아티팩트일 수 있기 때문입니다. 작업 로그에 지운 경로가 처음 20개까지 남고, 지운 캐시의 blob은 다음 `blobstore.compact` 작업이 회수합니다.
- group은 `.meta/prefixes.txt`에 404로 답합니다(멤버의 접두사 목록을 그대로 내주면 다른 멤버의 아티팩트가 없다고 오해하기 때문입니다). 병합한 메타데이터의 `Last-Modified`는 멤버가 오프라인이 되어 내용이 바뀌어도 뒤로 가지 않고 앞으로만 움직입니다.
- 업로드 경로의 이름에 `?`, `#`, `;`, 제어 문자(NUL 포함)나 UTF-8이 아닌 바이트(직접 만든 요청의 `%E9` 같은)가 있으면 400으로 거부합니다(조회는 막지 않으므로 이미 있는 파일은 계속 받을 수 있습니다). release 정책 저장소는 timestamp가 붙은 SNAPSHOT 버전(`1.4-20260101.000000-1`)도 400으로 거부합니다.

### npm 클라이언트 설정

레지스트리를 그룹 저장소로 지정합니다.

```bash
npm config set registry http://localhost:8081/repository/npm-all/
```

로그인하면 토큰이 발급되어 `~/.npmrc`에 저장됩니다. 사용자 이름과 비밀번호는 config.yaml의 사용자입니다.

```bash
npm login --registry http://localhost:8081/repository/npm-all/
```

`npm publish`는 그룹의 `writeMember`(npm-hosted)에 저장됩니다. 같은 버전을 다시 publish하면 거부됩니다(403).
`npm install`, `npm dist-tag`, `npm deprecate`, `npm unpublish`, `npm whoami`, `npm logout`(그 토큰을 폐기)을 지원하며, `npm search`(404)와 `npm audit`(501)은 지원하지 않습니다.
`npm ping`과 `npm audit`도 저장소 읽기 권한이 필요합니다(익명 읽기가 꺼져 있으면 토큰 필요). `npm login`은 토큰 없이 됩니다. 헬스 체크에는 `npm ping` 대신 인증 없이 200을 주는 `GET /service/rest/v1/status`를 쓰세요.
새 버전은 SemVer 2.0 형식(`1.2.3`, `1.2.3-beta.1`)이어야 하고 빌드 메타데이터(`3.0.0+a`처럼 `+…`)는 붙일 수 없습니다(400). npm은 `+…`를 무시하므로 `3.0.0`과 같은 버전이 둘이 되어 `npm install X@3.0.0`이 실패하기 때문입니다. dist-tag 이름은 버전이나 범위처럼 보이면(`1.0.0`, `^1`, `1.x`) 거부됩니다.
publish는 저장한 tarball도 검사합니다. gzip으로 압축한 tar여야 하고, 맨 위 디렉터리 바로 아래에 일반 파일 `package.json`이 하나만 있어야 하며, 그 `name`과 `version`이 publish 문서와 같아야 합니다. npm은 압축을 풀 때 경로의 첫 요소를 떼므로 맨 위 디렉터리 이름은 `npm pack`이 쓰는 `package/`가 아니어도 됩니다. `version`은 npm이 publish할 때처럼 앞의 `v`·`=`와 빌드 메타데이터(`+…`)를 뗀 값으로도 비교하므로, `package.json`에 `1.2.3+ci.5`처럼 적은 패키지도 `npm publish`로 올라갑니다. publish 문서의 새 버전마다 `versions.<버전>.name`도 패키지 이름과 같아야 합니다. 빈 파일, gzip이나 tar가 아닌 파일, `package.json`이 없거나 둘 이상인 tarball, `package.json`이 1 MiB를 넘는 tarball은 400입니다. 압축을 푼 크기는 publish 문서 하나에 든 새 tarball을 모두 합쳐 1 GiB까지만 읽고, 넘으면 400(`more than 1073741824 bytes uncompressed in this publish`)입니다. `npm publish`는 문서 하나에 버전 하나를 보내므로 tarball 하나가 1 GiB까지입니다. 버전 형식, `versions.<버전>.name`, shasum·integrity는 tarball을 읽기 전에 검사합니다. 처음 publish하는 패키지 이름은 npm이 새 패키지에 요구하는 대로 소문자여야 하고 `node_modules`, `favicon.ico`는 쓸 수 없습니다(400). 점(`.`)이나 밑줄(`_`)로 시작하는 이름, 214자를 넘는 이름, URL에 그대로 쓸 수 없는 문자가 든 이름은 이전부터 400입니다. 다만 npm처럼 scope 이름은 scope나 패키지 부분이 밑줄로 시작해도 되므로, `@acme/_internal` 같은 패키지도 publish할 수 있고 proxy와 group으로 받을 수 있습니다. scope나 패키지 부분이 점으로 시작하는 이름(`@acme/.x`)은 계속 400입니다. 이미 저장된 패키지는 대문자 이름이어도(예전 빌드에서 올린 경우) 새 버전을 계속 publish할 수 있고, `JSONStream`처럼 예전 규칙으로 만든 공개 패키지도 proxy와 group으로 그대로 받을 수 있습니다.
그룹은 멤버 순서상 그 패키지를 처음 가진 멤버 하나만으로 답합니다(hosted를 proxy보다 앞에 두세요). packument, 버전 문서, dist-tag, tarball이 모두 그 멤버의 것이고 뒤 멤버의 버전·dist-tag는 섞이지 않으며, 뒤 멤버에만 있는 버전의 tarball URL을 직접 요청해도 404입니다. 예를 들어 hosted에 `foo@1.0.0`이 있으면 proxy에 `foo@9.9.9`와 `beta` 태그가 있어도 그룹은 `1.0.0`만 보여 주고 `npm install foo@beta`는 실패합니다. hosted에 없는 이름은 proxy에서 받습니다.
그 패키지를 처음 가진 멤버보다 앞선 멤버가 "없음"(404)이 아닌 오류를 내면 그룹은 5xx(502 또는 그 멤버의 5xx)로, Offline이면 503으로 답합니다. hosted의 일시 오류나 점검 때문에 proxy에 있는 같은 이름의 공개 패키지가 대신 나가지 않게 하기 위해서입니다. 패키지를 가진 멤버보다 뒤에 있는 멤버는 조회하지 않으므로, hosted 뒤의 proxy가 장애이거나 Offline이어도 hosted 패키지는 계속 받을 수 있습니다(반대로 proxy를 hosted보다 앞에 두면 proxy 장애 때 그룹이 5xx를 냅니다).
tarball 요청은 packument를 읽지 않아도 됩니다. 그룹은 멤버 순서대로 그 tarball을 찾아 처음 가진 멤버로 답하고(hosted는 DB, proxy는 캐시에 있으면 캐시, 없으면 원격에서 그 tarball만 받음), tarball이 없는 멤버에게만 그 패키지를 가졌는지 packument로 묻습니다. 가졌다면 그 멤버가 주인이므로 404이고 뒤 멤버의 tarball은 나가지 않습니다. 마지막 멤버에게는 묻지 않습니다. 그래서 `npm ci`처럼 lockfile로 tarball만 받는 설치에는 packument가 필요 없고, proxy에 캐시된 tarball은 원격 레지스트리가 장애이거나 원격에서 패키지가 삭제된 뒤에도 proxy URL로 받을 때처럼 그룹으로 받을 수 있습니다. 마지막이 아닌 proxy 멤버에 그 tarball이 없으면 그 멤버의 packument로 판단합니다. 캐시가 만료되었는데 원격이 장애이면 만료된 캐시를 쓰고(주인이면 404), 원격이 404·410이면 그 멤버에는 패키지가 없는 것으로 보고 다음 멤버로 넘어갑니다(그룹 packument와 같은 판단). tarball이 캐시에 없고 원격에도 닿지 않으면 packument 캐시가 있어도 그룹은 502로 실패합니다.
`npm unpublish <패키지>@<버전>`은 그 버전을 지우고, `npm unpublish <패키지> --force`는 모든 버전과 dist-tag를 지워 패키지가 404가 됩니다. 저장소(그룹이면 `writeMember`)에 `delete` 권한이 있어야 합니다(없으면 403). proxy나 `writeMember`가 없는 group에서는 `405`(빈 `Allow` 헤더, 메시지 `unpublish is not supported in <유형> repository "<이름>": unpublish from its hosted repository`)로 실패하므로, npm이 "삭제했다"고 잘못 보고하지 않습니다. 지운 버전을 가리키던 dist-tag는 지워지고, `latest`가 지운 버전을 가리켰다면 남은 버전 중 가장 높은 정식 버전(정식 버전이 없으면 가장 높은 프리릴리스)으로 옮겨집니다. npm이 보낸 dist-tag 값은 쓰지 않습니다. npm은 읽어 간 패키지 문서에서 그 버전을 뺀 문서를 보내는데, 그사이 다른 사람이 새 버전을 올렸다면 그 버전까지 지우지 않도록 `409`로 거부하니 다시 실행하세요. npm은 읽어 간 문서의 `_rev`도 요청 주소(`/-rev/<rev>`)에 넣어 보냅니다. hosted 패키지 문서의 `_rev`는 그 문서에서 가장 나중에 게시된 버전의 게시 시각(밀리초)입니다. 요청이 지울 버전 중 이보다 나중에 게시된 버전, 즉 npm이 읽은 뒤 게시되었거나 지워졌다가 다시 게시된 버전이 있으면 아무것도 지우지 않고 `409`로 거부합니다. 문서를 보내지 않는 `npm unpublish <패키지> --force`(버전이 하나뿐인 패키지의 `npm unpublish <패키지>@<버전>`도 같습니다)도 이 규칙을 따릅니다. 그사이 다른 사람이 다른 버전을 지웠거나 dist-tag를 바꿨거나 `npm deprecate`를 한 것은 거부 사유가 아니므로, 서로 다른 버전을 지우는 `npm unpublish`를 동시에 여러 개 실행해도 모두 성공합니다. 이전 버전이 저장한 문서에는 `_rev`가 없어 npm이 `undefined`를 보내며, 이처럼 숫자가 아닌 `_rev`는 비교하지 않습니다. 그 패키지가 한 번 바뀌면(publish, dist-tag 변경, deprecate, 버전 삭제) `_rev`가 생깁니다. 이 문서는 10 MiB까지 받으며(넘으면 413), 그보다 큰 패키지의 버전은 웹 UI나 API로 지우세요("컴포넌트 삭제" 참고). hosted 패키지 문서는 `Cache-Control: no-cache`로 보내므로 npm이 캐시에 남은 옛 문서로 지운 버전을 고르지 않습니다. 지우지 않고 쓰지 말라고만 알리려면 `npm deprecate`를 쓰세요(설치할 때 경고가 나옵니다).

**baseURL 주의**: config.yaml의 `baseURL`은 클라이언트가 실제로 접속하는 레지스트리 주소(스킴, 호스트, 포트)와 정확히 같아야 합니다.
응답의 tarball URL이 `baseURL`로 만들어지기 때문입니다. 예를 들어 다른 컨테이너의 클라이언트가 접속한다면
`baseURL: "http://host.docker.internal:8081"`로 설정하고 레지스트리 주소도 같은 값(`http://host.docker.internal:8081/repository/npm-all/`)을 사용하세요.

**토큰 오류**: `~/.npmrc`의 토큰이 유효하지 않으면(예: 데이터 볼륨을 다시 만든 뒤) `anonymousRead`가 켜져 있어도 모든 요청이 401이 됩니다.
`npm login`을 다시 실행하거나 `~/.npmrc`에서 해당 `_authToken` 줄을 지우세요.

### PyPI 클라이언트 설정

pip는 그룹의 simple 인덱스를 사용합니다. `trusted-host`는 접속 호스트가 localhost가 아닐 때(예: `host.docker.internal`, LAN IP)만 필요합니다. HTTP는 localhost에 한해 pip가 경고 없이 허용합니다.

```bash
pip config set global.index-url http://localhost:8081/repository/pypi-all/simple/

# localhost가 아닌 호스트로 접속할 때
pip config set global.index-url http://192.168.0.10:8081/repository/pypi-all/simple/
pip config set global.trusted-host 192.168.0.10
```

`anonymousRead: false`이면 인덱스 URL에 인증 정보를 넣거나(`http://user:pass@host:8081/repository/pypi-all/simple/`) keyring을 사용하세요.

그룹은 멤버 순서상 그 프로젝트의 파일을 처음 가진 멤버 하나만으로 답합니다(hosted를 proxy보다 앞에 두세요). 프로젝트 페이지(HTML, PEP 691 JSON)와 파일 다운로드 모두 그 멤버의 것이고, 뒤 멤버의 파일은 섞이지 않으며 뒤 멤버에만 있는 파일의 URL을 직접 요청해도 404입니다. 그 멤버보다 앞선 멤버가 "없음"이 아닌 오류를 내면 5xx, Offline이면 503으로 답합니다. hosted에 없는 프로젝트는 proxy에서 받습니다. `/simple/` 프로젝트 목록은 Online 멤버의 이름을 합친 것입니다. 파일 다운로드는 그 파일을 처음 가진 멤버가 답하므로(proxy는 캐시에 있으면 인덱스를 읽지 않음), proxy에 캐시된 파일은 원격(PyPI)이 장애이거나 원격에서 프로젝트가 삭제된 뒤에도 그룹으로 받을 수 있습니다(`uv.lock`처럼 파일 URL로 설치할 때). 파일이 없는 멤버가 그 프로젝트를 가졌으면 404이고, 마지막 멤버에게는 묻지 않습니다. 캐시에 없는 파일은 proxy가 인덱스에서 URL과 sha256을 찾아야 하므로 인덱스가 필요합니다.

업로드는 twine으로 hosted 저장소(또는 writeMember가 있는 그룹)에 합니다. 업로드 URL은 반드시 `/`로 끝나야 합니다.

```bash
twine upload --repository-url http://localhost:8081/repository/pypi-hosted/ -u ci dist/*
```

사용자 이름 자리에 `__token__`을 쓰면 비밀번호 자리의 API 토큰이 누구 것이든 그 주인으로 인증됩니다(`twine upload -u __token__ -p cc_…`). 다른 사용자 이름은 토큰 주인과 같아야 합니다. 이 규칙은 모든 포맷의 Basic 인증에 같습니다.

업로드 검사: 파일은 `.whl`, `.tar.gz`, `.zip`만 받습니다(그 밖은 400). wheel 파일 이름은 `{이름}-{버전}(-{빌드 태그})-{python 태그}-{abi 태그}-{platform 태그}.whl` 형식이어야 하고, 빌드 태그는 숫자로 시작해야 합니다(아니면 400 `invalid wheel file name`). `version`은 PEP 440 버전이면 어떤 표기든 받습니다. `1.0-1`, `01.2`, `2.0a`, `1.0post2`, `1.0.dev`, `1.0RC2`는 정규형 `1.0.post1`, `1.2`, `2.0a0`, `1.0.post2`, `1.0.dev0`, `1.0rc2`로 저장하고 비교합니다. 앞에 붙은 `v`(`v1.3`)와 앞뒤 공백은 받지 않습니다(400 `invalid version "v1.3"`). 폼의 `version`과 파일 이름의 버전은 정규화해서 비교하므로 `1.0`과 `1.0.0`처럼 PEP 440에서 같은 버전이면 됩니다.

파일 내용도 검사합니다. wheel은 zip이어야 하고 `*.dist-info` 디렉터리가 하나뿐이어야 하며, 그 안의 `METADATA`의 `Name`과 `Version`이 폼의 이름·버전과 (정규화해서) 같아야 합니다. sdist(`.tar.gz`, `.zip`)는 맨 위 디렉터리의 첫 `PKG-INFO`가 이 릴리스의 `<이름>-<버전>/` 디렉터리(표기는 달라도 됩니다)에 있어야 하고, 그 `Name`과 `Version`이 같아야 합니다. 텍스트 파일, 확장자와 다른 종류의 아카이브, 다른 패키지나 다른 버전의 메타데이터가 든 파일은 400(`invalid distribution file: …` 또는 `the file's metadata is for …`)이고 아무것도 기록되지 않습니다. zip(wheel, zip sdist)은 항목 10,000개와 central directory 2 MiB까지만 검사할 수 있어 이를 넘으면 400입니다. 항목 수와 크기는 zip에 적힌 값만 믿지 않고 central directory를 직접 세어 확인합니다. `.tar.gz` sdist는 `PKG-INFO`가 압축을 푼 처음 512 MiB 안에 있어야 합니다(hatchling, flit, poetry 같은 빌드 도구는 `PKG-INFO`를 맨 끝에 넣습니다).

같은 파일을 다시 올리면 `File already exists.`(400)로 거부됩니다. 같은 파일인지는 이름이 아니라 정규화한 프로젝트 이름, 버전, wheel 태그로 판단하므로 `Foo_Bar-1.0.0`과 `foo-bar-1.0`처럼 표기만 다른 것도 중복입니다. wheel의 python·abi·platform 태그는 집합으로 비교하므로(`py3.py3`은 `py3`, `py3.py2`는 `py2.py3`과 같습니다) 압축 태그 표기만 다른 wheel과 숫자가 같은 빌드 태그(`01`과 `1`)의 wheel도 중복입니다. 한 릴리스에는 sdist를 하나만 올릴 수 있습니다(`.tar.gz`가 있으면 `.zip`은 400, PEP 527). 이름이 다른 기존 파일과 겹치면 `File already exists: this release already has "<기존 파일>" …`로 그 파일을 알려 줍니다. twine은 서버의 오류 메시지를 기본으로 숨기므로, 원인을 보려면 `twine upload --verbose ...`로 실행하세요.

twine이 보내는 `sha256_digest`, `md5_digest`, `blake2_256_digest` 중 받은 파일과 다른 것이 있으면 400입니다. `Requires-Python`은 폼 값보다 파일 안의 메타데이터(wheel `METADATA`, sdist `PKG-INFO`)를 우선하며, 폼 값이든 메타데이터 값이든 지정자로 올바르지 않으면(`banana`, `3.8` 등) 400입니다. 메타데이터는 첫 빈 줄까지의 헤더 부분만 읽으므로 긴 README(long_description)가 있어도 업로드는 거부되지 않습니다. 헤더 부분이 1 MiB를 넘으면 그 뒤의 헤더는 읽지 않습니다. 그 뒤에 있는 `Requires-Python`은 무시하고 폼 값을 쓰며, `Name`이나 `Version`이 그 뒤에 있으면 400입니다(빌드 도구는 둘을 맨 앞에 씁니다).

프로젝트 페이지(HTML, PEP 691 JSON)의 파일 링크는 호스트 없는 경로(`/repository/<저장소>/packages/…`, `baseURL`에 경로가 있으면 그 경로가 앞에 붙습니다)이므로, pip는 인덱스에 접속한 주소(스킴, 호스트, 포트, URL에 넣은 인증 정보)로 파일을 받습니다. 그래서 `localhost`와 `127.0.0.1`처럼 `baseURL`과 다른 이름으로 접속해도 파일 다운로드가 401이 되지 않습니다. 리버스 프록시가 경로 앞부분을 붙여 서비스한다면 `baseURL`에도 같은 경로를 두세요.

twine에는 삭제 명령이 없으므로 잘못 올린 버전은 웹 UI나 API로 지웁니다("컴포넌트 삭제" 참고). 그 버전의 파일(wheel, sdist)이 모두 지워지고, 프로젝트 페이지와 group에서 바로 사라집니다.

### Go 모듈 설정

`GOPROXY`를 그룹 저장소로 지정합니다.

```bash
go env -w GOPROXY=http://localhost:8081/repository/go-all
go env -w GONOSUMDB=example.com
```

사내 모듈은 공개 체크섬 DB(sum.golang.org)에 없으므로 `GONOSUMDB`에 경로 접두사를 넣으세요.
`GOPRIVATE`는 프록시를 거치지 않게 만들므로 Command Center의 hosted 모듈에는 쓰지 마세요.

hosted 저장소에는 `.mod`와 `.zip`을 HTTP PUT으로 올립니다(`.info`는 없으면 자동 생성).
zip 안의 모든 파일은 `모듈경로@버전/` 아래에 있어야 하며, 대문자는 `!소문자`로 인코딩한 경로를 씁니다.

```bash
curl -u ci -X PUT --data-binary @go.mod     http://localhost:8081/repository/go-hosted/example.com/hello/@v/v1.0.0.mod
curl -u ci -X PUT --data-binary @hello.zip  http://localhost:8081/repository/go-hosted/example.com/hello/@v/v1.0.0.zip
```

같은 버전 파일을 다시 올리면 409로 거부됩니다(`writePolicy: allow`이면 허용). `.mod`를 먼저 올렸다면 zip 안의 `go.mod`가 그 `.mod`와 바이트 단위로 같아야 하고, 다르면 409입니다. `go.mod`가 없는 zip에는 go 명령이 그런 zip에 쓰는 `module <모듈 경로>` 한 줄짜리 `.mod`만 맞습니다. `writePolicy: allow`이면 zip의 `go.mod`가 기존 `.mod`를 대신하는데, 이미 있는 파일을 바꾸는 것이므로 edit 권한이 필요합니다(없으면 403).

`@v/list`, `@latest`, `.info`는 `.zip`이 있는 버전만 보여 줍니다. `.mod`만 올라간 버전은 목록에 나오지 않고 `.info`도 404이지만(group은 다음 멤버에게 묻습니다), 이미 올라간 `.mod`는 직접 요청하면 받을 수 있습니다. zip은 Go 모듈 zip 규칙의 핵심을 검사합니다. 같은 이름의 항목이 둘 이상이거나 대소문자만 다른 이름이 겹치거나, 경로에 `\`·`:`나 빈 요소(`//`), `.`·`..` 요소, `con`·`nul`·`com1`처럼 Windows 예약 이름이 있으면 400입니다. `zip -r`이 만드는 디렉터리 항목(`모듈경로@버전/`, `sub/`)은 받지만, 같은 이름의 파일과 디렉터리가 함께 있거나 디렉터리 항목에 내용이 있으면 400입니다. 모든 항목을 끝까지 풀어 읽어서 CRC와 헤더에 적힌 크기가 실제와 맞는지 확인하고(다르면 400), 압축을 푼 크기 합계 500 MiB는 실제로 읽은 바이트로 지킵니다. 루트의 `LICENSE`와 `go.mod`는 각각 16 MiB까지입니다. 올린 파일(`.mod`·`.zip`·`.info`)의 검사는 서버 전체에서 동시에 2개까지만 하고, 나머지 업로드는 본문을 디스크에 받아 둔 채 차례를 기다립니다.

`.mod`와 zip 안의 `go.mod`는 go 명령이 읽지 못하는 형식이면 400입니다. 검사하는 것은 토큰(주석은 `//`만 쓰고, 따옴표 문자열은 그 줄에서 끝나야 함), 괄호 블록의 짝, `module` 지시어(정확히 하나, 경로 하나), `go` 지시어(많아야 하나, `1.21`·`1.21.0`·`1.22rc1` 같은 버전), 그리고 Go 1.26이 아는 지시어(`module`, `go`, `toolchain`, `godebug`, `require`, `exclude`, `replace`, `retract`, `tool`, `ignore`)만 쓰는지입니다. `go` 지시어가 1.26보다 새 버전이면 모르는 지시어도 받습니다(새 Go가 추가한 지시어일 수 있고, 이때는 go 명령도 새 Go를 요구합니다). 다른 지시어의 인자(`require`의 버전, `replace`의 형식 등)는 검사하지 않으며, 한 줄이 1 MiB를 넘으면 400입니다.

모듈 경로의 메이저 버전 접미사는 버전과 일치해야 합니다(`example.com/hello/v2`는 `v2.x.y`, `gopkg.in/yaml.v3`는 `v3.x.y`). 접미사는 `/v2`부터이며, `/v0`, `/v1`, `/v01`, `/v1.2`처럼 `v`와 숫자·점으로 끝나는 다른 경로는 go 명령처럼 400으로 거부합니다. 접미사가 없는 경로에 `v2` 이상을 올리려면 `+incompatible` 버전을 쓰며, 이 zip에는 루트 `go.mod`가 없어야 합니다.

잘못 올린 버전은 그 버전의 `.mod`·`.info`·`.zip` 중 하나에 `DELETE`를 보내 지웁니다. 세 파일이 함께 지워지며(`.mod`만 올라간 미완성 버전도 마찬가지), `delete` 권한이 필요합니다(`add`만으로는 403). 성공하면 204, 그런 버전이 없으면 404이고, group으로 보내면 `writeMember`에서 지웁니다. go 명령은 받아 둔 모듈을 캐시(`GOMODCACHE`)와 `go.sum`에 남기므로, 지운 버전 번호로 다른 내용을 올리지 마세요.

```bash
curl -u ci -X DELETE http://localhost:8081/repository/go-hosted/example.com/hello/@v/v1.0.0.zip
```

주의 사항:

- `anonymousRead: false`이면 go 명령이 인증 정보를 필요로 합니다. `~/.netrc`에 `machine <호스트> login <사용자> password <비밀번호>`를 설정하세요.
- 기본 정책(`allow_once`)에서 `.info`를 직접 올릴 때는 `.mod`/`.zip`보다 먼저 올려야 합니다. 나중에 올리면 자동 생성된 `.info`와 충돌해 409가 납니다.
- group 조회는 hosted 모듈 경로도 proxy.golang.org로 전달합니다(list, latest, 버전 쿼리). 따라서 사내 모듈 경로 이름이 외부 프록시에 노출됩니다.
- 폐쇄망에서는 공개 모듈에도 `GOSUMDB=off` 또는 `GONOSUMDB`를 설정하세요(sum.golang.org에 접속할 수 없습니다).
- 업로드 크기 제한: `.zip` 500 MiB, `.mod` 16 MiB, `.info` 1 MiB. 초과하면 413을 반환합니다.

### Docker 설정

Docker/OCI 레지스트리 API는 `/v2/`에서 제공합니다. 이미지 이름은 `<저장소 이름>/<이미지 경로>`입니다(호스트 하나, 경로 접두사. Nexus와 달리 저장소별 포트가 없습니다).

```bash
docker login <host>                              # 사용자 비밀번호 또는 API 토큰
docker push <host>/docker-hosted/team/app:1.0
docker pull <host>/docker-hosted/team/app:1.0
```

- 지금은 hosted만 지원합니다(proxy·group은 추후). 인증은 Distribution 토큰 방식(`/v2/token`, 5분 유효)이며 Basic 인증도 바로 받습니다. `GET /v2/`는 자격 증명 없는 요청에 익명 읽기가 켜져 있어도 401과 토큰 안내를 돌려줍니다. docker, crane, skopeo가 토큰 주소를 그 응답에서 찾기 때문이며, 익명 읽기가 켜진 저장소는 그 뒤 익명 토큰으로 로그인 없이 pull 됩니다. push에는 add, 매니페스트 삭제(`DELETE /v2/<이름>/manifests/<digest>`)에는 delete 권한이 필요합니다.
- `writePolicy`: `allow`는 태그 덮어쓰기 허용, `allow_once`는 이미 있는 태그를 다른 이미지로 바꾸지 못합니다(409). `deny`는 읽기 전용입니다.
- 매니페스트는 받은 바이트 그대로 저장되며(최대 4 MiB. schema2, manifest list, OCI manifest, OCI index를 받고 schema1은 거부), 참조하는 blob이 모두 있어야 저장됩니다. 레이어는 내용 주소 저장소에서 중복 제거되고, 업로드 하나는 `maxUploadMiB`를 넘을 수 없습니다. 중단된 업로드 세션은 24시간 뒤 정리됩니다.
- 매니페스트를 digest로 삭제하면 태그와 더 이상 참조되지 않는 레이어 행이 함께 지워지고, blob은 compact 작업이 회수합니다. 태그로는 삭제할 수 없습니다(400). Cleanup 정책은 docker 저장소에 적용되지 않습니다.
- 리버스 프록시 뒤에서는 `baseURL`의 호스트가 토큰의 service 값이 되므로 클라이언트가 접속하는 주소와 같아야 합니다. HTTPS가 아니면 docker 데몬에 insecure-registries 설정이 필요합니다(localhost 제외).

### Cargo 설정

`~/.cargo/config.toml`에 레지스트리를 추가하고 crates.io를 Command Center로 대체합니다.

```toml
[registries.commandcenter]
index = "sparse+http://localhost:8081/repository/cargo-all/"

[source.crates-io]
replace-with = "commandcenter"
```

publish·yank에는 API 토큰이 필요합니다. 서버에서 발급한 뒤 `cargo login`으로 저장하세요.

```bash
docker compose exec commandcenter commandcenter token create -user ci
cargo login --registry commandcenter
cargo publish --registry commandcenter
```

같은 버전을 다시 publish하면 409로 거부됩니다. `cargo yank`/`cargo yank --undo`를 지원합니다. yank한 버전은 새로 의존성을 고를 때만 빠지고 `Cargo.lock`에 고정된 빌드는 계속 받습니다. 아예 지우려면(예: 잘못 올린 crate) 웹 UI나 API로 삭제하세요("컴포넌트 삭제" 참고). 삭제한 버전은 인덱스와 다운로드에서 바로 사라지므로, 그 버전을 고정한 빌드는 실패합니다.
publish는 `.crate`를 검사합니다. gzip으로 압축한 tar여야 하고, 모든 항목이 `<이름>-<버전>/` 아래에 있어야 하며(절대 경로와 `..` 불가), 링크나 sparse 항목은 받지 않습니다. `<이름>-<버전>/Cargo.toml`(1 MiB 이하)이 있어야 하고, 그 `[package]` 표의 `name`과 `version`이 publish 메타데이터와 대소문자까지 같아야 합니다. 두 값은 한 줄 문자열이어야 하므로 `version.workspace = true` 같은 workspace 상속이 남아 있으면 거부합니다(`cargo publish`는 실제 값으로 바꿔 써서 올리므로 해당하지 않습니다). 위반하면 모두 400입니다. 저장된 crate는 `crates/<이름>/<버전>.crate` 경로로도 받을 수 있습니다(UI와 API가 이 주소를 알려 줍니다).
publish 메타데이터 중 인덱스에 그대로 들어가는 의존성 항목도 검사해, cargo가 읽지 못하는 줄이 인덱스에 남지 않게 합니다. 의존성 이름과 `explicit_name_in_toml`(영문자나 `_`로 시작하고 영문자·숫자·`-`·`_`만, 64자 이하), `version_req`(cargo의 semver 요구 조건: `^1.2`, `~1`, `>=1, <2`, `=1.0.0-rc.1`, `1.*`, `*` 등), `kind`(`normal`, `dev`, `build`), `target`(`cfg(...)` 식이나 `x86_64-unknown-linux-gnu` 같은 타깃 이름), `registry`(절대 URL)가 맞지 않으면 400입니다. crates.io와 달리 `*` 요구 조건과 다른 레지스트리의 의존성은 허용합니다.
Windows 장치 이름(`con`, `prn`, `aux`, `nul`, `com1`–`com9`, `lpt1`–`lpt9`)과 표준 라이브러리 이름(`std`, `core`, `alloc`, `proc_macro`, `test`)으로는 새 crate를 만들 수 없습니다(400, 대소문자와 `-`/`_`는 구분하지 않음). 이미 그 이름으로 올라가 있는 crate는 crates.io처럼 계속 새 버전을 올릴 수 있습니다.
crate 이름은 올린 표기(대소문자 포함) 그대로 인덱스, 웹 UI(검색 결과와 컴포넌트·asset 화면의 `cargo add` 예시), 패키지 API(`/-/api/repositories/<저장소>/packages`)와 활동 이벤트에 나옵니다. cargo는 의존성 이름을 대소문자까지 맞춰 찾기 때문입니다. 저장 경로(`crates/<이름>/<버전>.crate`)는 소문자이고, 인덱스·다운로드·yank·패키지 API 조회는 대소문자를 가리지 않습니다. proxy에 캐시된 crate도 웹 UI에는 캐시해 둔 인덱스 파일에서 읽은 올린 표기로 나오고, 인덱스 파일이 캐시에 없으면 소문자 이름으로 나옵니다("컴포넌트는 파생 값" 참고). 패키지 API는 packages 테이블을 읽으므로 proxy의 crate는 돌려주지 않습니다.
그룹 인덱스는 멤버 순서상 그 crate를 처음 가진 멤버의 인덱스를 그대로 냅니다(멤버끼리 합치지 않으므로 hosted를 proxy보다 앞에 두세요). 멤버가 "없음"이 아닌 오류를 내면 다음 멤버로 넘어가지 않고 5xx로 답합니다. 건너뛰면 오래된 버전이 조용히 선택될 수 있기 때문입니다.
인덱스 응답에는 hosted, proxy, group 모두 `ETag`가 붙고, 맞는 `If-None-Match`에는 304로 답합니다. proxy의 ETag는 캐시한 원격 인덱스 파일의 내용을, group의 ETag는 응답한 멤버와 그 멤버의 ETag를 따르므로, 내용이 바뀌거나 다른 멤버가 응답하게 되면 달라집니다. `Last-Modified`는 보내지 않고 `If-Modified-Since`는 무시합니다. yank는 올린 시각을 바꾸지 않고 group은 응답하는 멤버가 바뀔 수 있어, 시각으로 비교하면 바뀐 인덱스에 304가 나갈 수 있기 때문입니다. cargo는 `ETag`가 있으면 그것만 씁니다.

- `anonymousRead: false`인 저장소는 config.json이 `auth-required: true`를 알립니다. cargo 1.74 이상은 읽기에도 토큰이 필요하므로, `cargo login --registry commandcenter` 실행 시 `token create`로 출력된 토큰을 입력하세요.
- 설정 파일의 `baseURL`은 `index = "sparse+http://…"`에 쓰는 주소와 같아야 합니다. config.json의 `dl`·`api`가 이 값으로 만들어집니다.
- proxy 저장소의 `remote`는 `sparse+` 접두사를 뗀 sparse index URL입니다. 예: `https://index.crates.io/`

## 운영

| 작업 | 명령 |
|---|---|
| 로그 | `docker compose logs -f` |
| 메모리 확인 | `docker stats commandcenter` |
| 사용하지 않는 blob 정리 | `docker compose exec commandcenter commandcenter gc` |
| API 토큰 발급 | `docker compose exec commandcenter commandcenter token create -user ci` |
| 비밀번호 해시(첫 가져오기용) | `docker run --rm -i ghcr.io/sungjinseo/commandcenter:1.2.3 hash-password` |
| 관리자 비상 복구 | `docker compose exec -T commandcenter commandcenter user reset-password -user admin -admin`(서버가 멈춰 있으면 `docker compose run --rm -T commandcenter user reset-password -user admin -admin`) |
| 비밀값 키 교체 | 서버를 멈추고 `docker compose run --rm -v "${PWD}/new.key:/run/new.key:ro" commandcenter secret-key rotate -new-key-file /run/new.key`, 그다음 키 설정을 새 키로 바꿔 `docker compose up -d`("저장된 비밀값 암호화"의 키 교체 참고) |
| 버전 확인 | `docker compose exec commandcenter commandcenter version` (릴리스 이미지·바이너리는 태그 이름(예: `v1.2.3`), 직접 빌드한 이미지는 빌드 인자 `VERSION`의 값, 지정하지 않았으면 `dev`) |

Kubernetes에서는 `docker compose exec commandcenter` 대신 `kubectl -n <네임스페이스> exec commandcenter-0 --`를 씁니다("Kubernetes"의 "동작 방식" 참고).

### 로그

표준 출력에 JSON 한 줄씩 기록합니다. 요청마다 `id`, `method`, `path`, `status`, `bytes`, `ms`, `user`, `remote` 필드가 있습니다.
클라이언트가 보낸 `X-Request-Id`는 1–64자의 `[A-Za-z0-9._-]`일 때만 쓰고, 아니면 서버가 새로 만듭니다. 500 오류 메시지에 나오는 request id로 로그를 검색할 수 있습니다.

### 메트릭

`GET /-/metrics`는 `admin` 사용자만 호출할 수 있습니다(JSON).

```bash
curl -u admin http://localhost:8081/-/metrics
```

필드: `heapAllocBytes`, `heapSysBytes`, `sysBytes`, `numGC`, `goroutines`, `rssBytes`(Linux에서만 값이 있음).

### Cleanup 정책

오래되었거나 더 이상 내려받지 않는 컴포넌트를 자동으로 지우는 규칙입니다. 메뉴는 OPERATIONS → Cleanup 정책입니다. 정책은 이름, 대상 포맷(`all` 또는 다섯 포맷 중 하나)과 아래 기준으로 이루어지며, 저장소 폼의 **Cleanup policy** 선택(저장소 정의의 `cleanupPolicy`)으로 hosted·proxy 저장소에 연결합니다. 정책의 포맷은 `all`이거나 저장소의 포맷과 같아야 하고, 저장소가 쓰는 정책은 지울 수 없습니다(409, 저장소 이름을 알려 줌).

| 기준 | 의미 |
|---|---|
| `componentAgeDays` | 컴포넌트의 **가장 새로운 에셋**이 N일보다 오래전에 만들어졌다. 0이면 사용 안 함. |
| `lastDownloadedDays` | 컴포넌트의 가장 최근 다운로드가 N일보다 오래되었다. 한 번도 받지 않았으면 생성 시각을 씁니다. 0이면 사용 안 함. |
| `releaseType` | `any`, `release`, `prerelease`(SNAPSHOT 또는 semver 프리릴리스). |
| `namePattern` | `group:name`에 맞추는 RE2 정규식(선택, 200자 이하). |

- 두 기간 중 **하나 이상**은 정해야 하며, 정한 기준은 모두 **AND**로 결합합니다. 모든 기준을 만족하는 컴포넌트만 후보입니다.
- **미리보기**(정책 화면의 "후보 목록 보기")는 후보 개수·용량과 최대 500개의 목록을 보여 줍니다. 저장소마다 에셋을 20 000개까지만 훑으므로 목록이 잘렸거나 저장소가 그보다 크면 `truncated`가 켜지고, 이때 개수와 용량은 **하한**입니다.
- **Cargo hosted 저장소는 제외**됩니다. `Cargo.lock`에 고정된 crate를 자동으로 지우지 않기 위해서입니다. yank로 숨기거나, 지울 버전은 웹 UI·API로 하나씩 지우세요("컴포넌트 삭제" 참고). 정책은 Cargo hosted를 건너뛰고 UI에도 안내가 나옵니다. Cargo proxy는 대상입니다.
- hosted의 `writePolicy`는 정리에서 **무시**됩니다. `deny`인 저장소에서도 정리는 지웁니다.
- 정책은 저장만으로 아무것도 지우지 않습니다. **`repository.cleanup` 예약 작업**이 실행할 때만 지우며(다음 절), 한 번에 컴포넌트 5 000개까지, 개수와 용량을 작업 로그와 활동에 기록합니다. proxy는 캐시된 행만 지우고 다음 요청 때 다시 받아 옵니다.
- 작업 실행도 저장소마다 **한 번에 에셋 20 000개**까지만 훑습니다. 큰 저장소는 작업·저장소별 **스캔 위치**(settings 테이블)를 기억해 다음 실행이 마지막으로 훑은 컴포넌트 다음부터 이어 가고, 끝에 닿으면 처음으로 돌아갑니다. 그래서 여러 번 실행하면 저장소 전체를 차례로 평가합니다. 작업 로그에 어디서부터 몇 개를 훑었고 다음 실행이 어디서 시작하는지가 나옵니다. 작업을 지우면 그 위치도 지웁니다.
- 평가 직전에 메모리에 모인 다운로드 수를 먼저 DB에 반영하므로 방금 내려받은 컴포넌트도 보호됩니다.
- **평가한 것만 지웁니다.** 삭제는 평가 때 본 에셋 목록, 가장 새로운 생성·수정 시각, 마지막 다운로드 시각을 삭제 트랜잭션 안에서 다시 확인하고, 그 뒤로 에셋이 더해졌거나(예: 새 SNAPSHOT 빌드) 다시 올라왔거나 내려받아졌으면 그 컴포넌트는 지우지 않고 "평가 후 변경"으로 로그에 남깁니다(실패로 치지 않음).
- 실행 중에도 편집을 따릅니다. 컴포넌트마다 저장소 정의를 다시 보고(메모리), 정책은 50개마다 또는 정책이 수정될 때 다시 읽어, 저장소가 바뀌었거나 정책이 해제·변경되었으면 그 저장소의 삭제를 멈춥니다.
- 지운 에셋의 **blob은 바로 반환되지 않습니다.** 다음 `blobstore.compact` 작업이 회수합니다.
- 설정 PUT에서 `cleanupPolicy`를 빼면 기존 값이 유지됩니다.

### 예약 작업

OPERATIONS → 예약 작업(Tasks)에서 만들고 편집하고 지웁니다. 작업은 한 번에 하나씩 실행하며, 실행 중에 시간이 된 다른 작업은 다음 확인(30초마다) 때 실행됩니다.

| 종류 | 설명 | 파라미터 |
|---|---|---|
| `blobstore.compact` | 쓰이지 않는 blob과 1시간 넘은 임시 파일 정리(`commandcenter gc`와 같음) | `blobStore`(선택, 한 store만. 비우면 모든 store. 없는 이름은 만들 때 400) |
| `repository.cleanup` | 정책이 연결된 저장소를 정리 | `repos`(비우면 정책이 있는 모든 저장소) |
| `repository.rebuild-metadata` | hosted: 모든 GA와 SNAPSHOT 버전의 `maven-metadata.xml`을 에셋에서 다시 만들고, 아티팩트가 없는 GA의 문서에서 버전을 지움(`<plugins>`는 남김). proxy: 이전 버전이 캐시한, Maven 파일이 아닌 경로의 캐시(디렉터리 목록, `.index/`)를 지움 | `repo`(Maven hosted 또는 proxy) |

첫 시작 때 작업이 하나도 없으면 `compact-blobstore`(`blobstore.compact`, `daily:03:00`, 사용)를 만듭니다. 이 기본 작업은 지울 수 없습니다(`DELETE`는 409). 지우면 재시작해도 다시 생기지 않아 compaction이 조용히 멈추기 때문이며, 끄고 싶으면 "사용"을 끄세요.

**실패 알림**: 작업의 `notifyEmail`에 주소를 넣으면 실행이 Failed로 끝날 때 메일을 보냅니다("이메일 알림" 참고). 편집 화면의 "실패 알림 이메일" 입력란에 쉼표나 줄바꿈으로 구분해 적습니다.
**일정 문법**

| 일정 | 뜻 |
|---|---|
| `manual` | 지금 실행으로만 |
| `once:<RFC3339>` | 한 번, 예: `once:2026-11-01T02:00:00+09:00`. 오프셋이 없으면(`once:2026-11-01T02:00`) 서버 시간대의 시각입니다. 화면은 오프셋 없이 서버 시간대로 보내고, 시각을 바꾸지 않고 저장하면 원래 값을 그대로 둡니다. |
| `hourly:<MM>` | 매시 MM분 |
| `daily:<HH:MM>` | 매일 |
| `weekly:<mon..sun>:<HH:MM>` | 매주 해당 요일 |
| `monthly:<1-28>:<HH:MM>` | 매월 해당 일(29일 이후는 쓸 수 없음) |

- 시각은 서버의 시간대 기준이며 환경변수 `TZ`로 정합니다(기본 UTC). 예: compose의 `environment`에 `TZ: Asia/Seoul`. 서머타임으로 존재하지 않는 시각은 그 직후 첫 유효한 시각에 실행합니다.
- **지금 실행**은 비동기입니다. 요청은 바로 202를 돌려주고 작업은 뒤에서 돌며, 화면(또는 `GET /-/api/tasks/{id}`)의 상태가 Running에서 바뀌면 끝난 것입니다. 다른 작업이 실행 중이면 409("a task is already running", `POST /-/api/gc`도 같음)이고 응답의 `running`·`runningName`이 그 작업을 알려 줍니다. 수동 실행은 다음 예정 시각(`next_run`)을 바꾸지 않습니다.
- **중지**: 실행 중인 작업은 상세 화면의 "중지"(또는 `POST /-/api/tasks/{id}/stop`, 관리자)로 멈춥니다. 실행 중이면 202, 아니면 409이며, 작업은 진행 중인 단계(cleanup은 지우던 컴포넌트)를 마치고 멈춰 결과가 **Stopped**로 남습니다.
- 실행 하나는 최대 6시간이며 넘으면 취소됩니다. 실행 기록을 저장하지 못하면(DB 오류) 다음 예정 시각을 메모리에서 넘겨 틱마다 다시 실행하지 않습니다.
- 실행 로그는 최대 200줄·64 KiB이고 넘치면 **앞부분과 뒷부분**만 남기고 가운데를 생략했다는 줄을 넣습니다. 그래서 마지막 줄(성공·실패)은 항상 보입니다. 로그는 시스템 로그에도 함께 기록됩니다.
- 기존 `POST /-/api/gc`는 compact 실행의 별칭으로 남아 있습니다. "지금 실행"과 달리 실행이 끝난 뒤 200으로 응답하며(실패하면 500), `removed`와 `bytes`는 지운 blob의 수와 바이트 수입니다(작업의 `lastRemoved`·`lastBytes`와 같음).

### 로그·활동

- **로그**: SYSTEM → 시스템 상태·로그에서 서버 로그를 봅니다. 최근 **2 000건 또는 1 MiB** 중 먼저 닿는 한도까지 메모리에 보관하며, 재시작하면 사라집니다(표준 출력의 JSON 로그는 그대로 `docker compose logs`로 남습니다). 수준(INFO/WARN/ERROR)과 텍스트로 거르고, "다운로드"는 `commandcenter.log` 텍스트 파일을 내려줍니다(`GET /-/api/logs`, `/-/api/logs/download`, 관리자 전용).
- 로그에는 비밀 값이 들어가지 않습니다. 요청 경로의 토큰은 가려지고 NUL 같은 제어 문자는 `\x00` 꼴로 바뀌어 기록되며, proxy 원격 URL의 **자격 증명(userinfo)과 쿼리 값은 제거**하며, 인증 헤더는 기록하지 않습니다. 작업 로그에 나오는 URL도 같은 처리를 합니다.
- **활동(Events)**: 대시보드의 "최근 운영 활동"과 `GET /-/api/events`(관리자, 최대 200건씩, `before`·`kind`로 페이징·필터). DB에 최근 10 000건만 남기며, 기록은 비동기라 큐(1 000건)가 가득 차면 요청을 막지 않고 그 이벤트를 버립니다. 종류:

  | kind | 시점 |
  |---|---|
  | `upload` | hosted 쓰기 성공(Maven 주 아티팩트, npm publish, PyPI 업로드, Go zip, Cargo publish) |
  | `delete` | 에셋·컴포넌트 삭제(Maven `DELETE`, `npm unpublish`, Go `DELETE`, 웹 UI·API, Cleanup). npm·PyPI·Go·Cargo는 버전마다 한 건이며 지운 사람과 파일 수(`assets=N`)가 남습니다 |
  | `repo.create` / `repo.update` / `repo.delete` | 저장소 변경 |
  | `role.create` / `role.update` / `role.delete` | 역할 변경. 저장소를 지워 그 저장소 권한이 빠진 역할도 `role.update`(저장소 이름, `removed repo:<이름>`)로 남습니다 |
  | `token.create` / `token.revoke` | API 토큰 발급·폐기 |
  | `task.run` | 작업 실행(결과와 요약) |
  | `cleanup` | 정리 결과(개수, 용량) |
  | `policy.create` / `policy.update` / `policy.delete` | 정책 변경 |

### 다운로드 수·요청 추이

- 에셋 본문을 GET으로 성공적으로 받을 때마다(HEAD는 제외) 그 에셋의 다운로드가 1 늘고 마지막 다운로드 시각이 갱신됩니다. proxy가 캐시를 채우는 요청도 셉니다. 컴포넌트 상세의 기본 정보와 Assets 탭, Browse 파일 목록에 표시되며 Cleanup의 `lastDownloadedDays`가 이 값을 씁니다.
- 요청마다 DB에 쓰지 않습니다. 카운터는 메모리에 모았다가 **30초마다**(또는 항목이 10 000개에 닿으면, 종료할 때) 한 트랜잭션으로 반영합니다. 그래서 화면의 값은 최대 30초 늦습니다. 주기는 숨은 설정 `metricsFlushSeconds`(1..3600, 기본 30)로 바꿉니다.
- 요청 추이는 `/repository/<이름>/…` 요청 수와 응답 바이트를 시간 단위(UTC)로 집계해 대시보드의 7일 차트와 시스템 상태의 24시간 차트에 그립니다(`GET /-/api/stats/requests?range=24h|7d|30d&repo=`). 24h는 시간별, 7d·30d는 일별이며 **90일**이 지난 집계는 하루 한 번 지웁니다.

### blob 정리 (`commandcenter gc`)

어떤 자산에도 참조되지 않는 blob 파일(업로드 실패나 덮어쓰기로 남은 것)을 지웁니다. 수정된 지 1시간이 안 된 blob은 진행 중인 업로드를 보호하기 위해 지우지 않습니다. 결과는 `removed N unreferenced blobs`로 출력됩니다.

쓰기 도중 프로세스가 강제 종료되면(SIGKILL, OOM, 중지 대기 시간 초과) store의 `tmp/`에 미완성 임시 파일(`up-*`)이 남습니다. 1시간 넘게 수정되지 않은 임시 파일은 GC 때와 서버 시작 때 지웁니다. `gc`, `token create` 같은 CLI 명령도 시작할 때 같은 정리를 합니다. 진행 중인 쓰기는 늦어도 몇 분마다 파일을 갱신하므로, 서버가 돌고 있을 때 CLI를 실행해도 쓰는 중인 파일은 지워지지 않습니다. 지운 임시 파일 수는 위 결과에 들어가지 않습니다. compact 작업이 지운 경우에는 서버 로그에 `removed stale temp files`로 남습니다.

### 백업과 복원

Kubernetes의 명령은 "Kubernetes에서 백업"에 있습니다. 원칙은 이 절과 같습니다.

데이터는 named volume에 있습니다. 볼륨 이름은 기본 `commandcenter-data`이며 `COMMANDCENTER_VOLUME`으로 바꿀 수 있습니다. 이전 repolite 설치의 데이터를 쓰려면 `COMMANDCENTER_VOLUME=repolite_repolite-data`(또는 `docker volume ls`에 보이는 이름)를 지정하세요.

- **백업 대상**: `meta.db`(저장소 정의, 사용자, 역할, 토큰 해시, 설정, 파일 목록)와 모든 blob store입니다. default store는 같은 볼륨의 `blobs/`에, 경로를 비워 만든 이름 붙은 store는 같은 볼륨의 `blobstores/<이름>`에 있습니다. 절대 경로로 만든 store는 그 경로에 있으니 따로 챙기세요. `meta.db`와 blob은 같은 시점의 것이어야 합니다. `config.yaml`도 함께 보관하세요.
- **백업 파일 보호**: `meta.db`에는 proxy 원격 인증, LDAP bind, SMTP 비밀번호가 들어 있고, 비밀값 키를 정하지 않았다면 평문입니다("저장된 비밀값 암호화" 참고). 백업 파일도 서버와 같은 수준으로 접근을 제한하세요. 키를 정했다면 키도 백업하되 `meta.db` 백업과 다른 곳에 두세요. 키가 없으면 복원한 서버가 시작하지 않습니다.
- **실행 중인 볼륨을 그대로 복사하면 백업이 되지 않습니다.** `cp`나 `tar`가 `meta.db`를 읽는 동안 내용이 바뀌거나, `meta.db`가 아직 복사되지 않은 blob을 가리켜서 복원한 뒤 그 파일들이 404가 됩니다(`allow_once` 저장소에는 같은 파일을 다시 올릴 수도 없습니다).

일관된 백업 방법은 두 가지입니다.

1. **멈추고 복사(가장 단순)**: 아래 명령처럼 서버를 멈추고 볼륨을 통째로 복사한 뒤 다시 시작합니다. 중지와 기동은 보통 1초 안쪽이지만 복사하는 동안은 서비스가 멈춥니다.
2. **무중단**: 먼저 `meta.db`를 SQLite의 online backup API나 `VACUUM INTO`로 스냅샷하고, **그다음에** blob 디렉터리를 복사합니다. blob은 내용 주소 방식이라 한 번 쓰면 바뀌지 않으므로 스냅샷 뒤에 생긴 blob이 섞여도 무해합니다(다음 compact 작업이 정리합니다). 순서를 바꾸면 안 됩니다.
   - 서버 이미지에는 셸과 `sqlite3`가 없습니다. 같은 볼륨을 마운트한 다른 컨테이너에서 **서버와 같은 UID 65532로** 실행하세요(다른 사용자로 실행하면 그 사용자 소유의 파일이 볼륨에 남아 서버가 열지 못할 수 있습니다). 예: `sqlite3 /data/meta.db "VACUUM INTO '/data/meta-snapshot.db'"`. 그다음 `blobs/`(와 이름 붙은 store)를 스냅샷 파일과 함께 복사하고, 볼륨 안의 스냅샷은 지웁니다(`VACUUM INTO`는 이미 있는 파일에 쓰지 않습니다).
   - 백업하는 동안에는 compact 작업(`commandcenter gc` 포함)이 돌지 않게 하세요. 스냅샷 뒤에 지워진 파일의 blob을 compact가 회수하면 백업에서 빠집니다. 기본 compact 작업은 매일 03:00(서버 시간대)에 돕니다.
   - 복원할 때는 스냅샷을 `meta.db`라는 이름으로 두고, `meta.db-wal`·`meta.db-shm` 없이 시작합니다.

아래는 1번(멈추고 복사) 방법의 명령입니다.

**백업 (PowerShell)**

```powershell
docker compose stop
docker run --rm -v commandcenter-data:/data -v "${PWD}:/backup" busybox tar czf /backup/data.tgz /data
docker compose start
```

**백업 (Git Bash)** — Git Bash는 컨테이너 안 경로(`/backup`, `/data`)를 윈도우 경로로 바꾸므로 `MSYS_NO_PATHCONV=1`과 `pwd -W`가 필요합니다.

```bash
docker compose stop
MSYS_NO_PATHCONV=1 docker run --rm -v commandcenter-data:/data -v "$(pwd -W)":/backup busybox tar czf /backup/data.tgz /data
docker compose start
```

Linux/macOS는 `-v "$PWD":/backup`을 쓰면 됩니다.

**복원** — 오래된 `meta.db-wal`/`-shm` 파일이 섞이지 않도록 반드시 빈 볼륨에서 시작합니다. `docker compose down -v`는 데이터 볼륨을 삭제하므로 백업 파일을 먼저 확인하세요.

```powershell
docker compose down -v
docker compose create
docker run --rm -v commandcenter-data:/data -v "${PWD}:/backup" busybox tar xzf /backup/data.tgz -C /
docker compose up -d
```

Git Bash에서는 복원의 `docker run` 앞에 `MSYS_NO_PATHCONV=1`을 붙이고 `-v "$(pwd -W)":/backup`을 씁니다.

### 릴리스 이미지와 바이너리

`v`로 시작하는 태그를 푸시하면 GitHub Actions(`.github/workflows/release.yml`)가 CI를 다시 통과시킨 뒤 다음을 만듭니다.

- **이미지** `ghcr.io/sungjinseo/commandcenter`(linux/amd64, linux/arm64). `v1.2.3`이면 `1.2.3` 태그가 붙고, 움직이는 태그 `1.2`, `1`, `latest`는 이 버전이 각 범위(1.2.x, 1.x, 전체)의 정식 릴리스 중 가장 높을 때만 이 버전으로 옮겨집니다. 예를 들어 `v1.3.0` 다음에 유지보수 릴리스 `v1.2.5`를 내면 `1.2.5`와 `1.2`만 바뀌고 `1`과 `latest`는 1.3.0에 남습니다. `v0.x`에는 `0` 태그가 없고, `v1.3.0-rc.1`처럼 `-`가 들어간 프리릴리스에는 자기 태그(`1.3.0-rc.1`)만 붙으며 가장 높은 버전을 따질 때도 세지 않습니다. GitHub Release의 "Latest" 표시도 `latest` 태그와 같은 버전에 붙습니다.
- **운영에서는 `1.2.3`처럼 정확한 버전을 고정하세요.** 새 버전이 DB를 바꾼 뒤에는 이전 버전으로 되돌리기 어려우므로 `latest`나 `1` 같은 움직이는 태그로 의도치 않게 올라가면 안 됩니다.
- **Helm 차트** `oci://ghcr.io/sungjinseo/charts/commandcenter`: 차트 버전과 appVersion은 릴리스 버전(`1.2.3`)이고, 기본 이미지 태그도 그 버전입니다(`helm show chart oci://ghcr.io/sungjinseo/charts/commandcenter --version 1.2.3`으로 확인). `--version`으로 고정해서 씁니다("Kubernetes" 참고). 차트도 이미지와 같은 방법으로 서명하므로, 아래 `cosign verify`의 이미지 이름을 `ghcr.io/sungjinseo/charts/commandcenter:1.2.3`으로 바꿔 확인합니다.
- compose 파일은 이 이미지를 씁니다. `.env`의 `COMMANDCENTER_VERSION`에 버전(`1.2.3`, `v` 없이)을 적고, 사내 레지스트리로 옮긴 사본을 쓰려면 `COMMANDCENTER_IMAGE`에 그 이름(태그 없이)을 적습니다("빠른 시작" 참고). 직접 빌드했거나 사본을 쓴다면 이 가이드의 `docker run` 명령에 나오는 `ghcr.io/sungjinseo/commandcenter:1.2.3`도 그 이름으로 바꿔 쓰세요.
- **SBOM과 provenance**: 이미지에 SPDX SBOM과 빌드 provenance 증명이 함께 올라갑니다. `docker buildx imagetools inspect ghcr.io/sungjinseo/commandcenter:1.2.3 --format '{{ json .SBOM }}'`(provenance는 `.Provenance`)로 확인합니다.
- **이미지 서명**: 이미지는 cosign으로 키 없이(keyless) 서명합니다. 서명 인증서에는 서명한 워크플로와 태그(`https://github.com/sungjinseo/commandcenter/.github/workflows/release.yml@refs/tags/v1.2.3`)가 적히고, 서명은 Sigstore의 공개 투명성 로그에 남습니다. 서명은 태그가 가리키는 이미지(여러 플랫폼을 묶은 인덱스)의 digest에 붙고, 플랫폼별 이미지의 digest에는 붙지 않습니다. cosign 3 이상으로 다음처럼 확인합니다.

  ```bash
  cosign verify ghcr.io/sungjinseo/commandcenter:1.2.3 \
    --certificate-identity https://github.com/sungjinseo/commandcenter/.github/workflows/release.yml@refs/tags/v1.2.3 \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
  ```

- **바이너리**: GitHub Release에 `commandcenter-v1.2.3-linux-amd64.tar.gz`, `commandcenter-v1.2.3-linux-arm64.tar.gz`(이미지 안의 바이너리와 같은 파일, 라이선스·고지 파일 포함)와 `SHA256SUMS`가 올라갑니다. 받은 뒤 `sha256sum -c --ignore-missing SHA256SUMS`로 확인하세요.

### 업그레이드

1. 올릴 버전의 [CHANGELOG](../CHANGELOG.md)를 읽습니다. 설정이나 운영 절차가 바뀌면 거기에 적습니다.
2. 백업합니다("백업과 복원" 참고). 업그레이드한 뒤 되돌릴 수 있는 방법은 이 백업뿐입니다.
3. 새 버전으로 바꿉니다. `.env`의 `COMMANDCENTER_VERSION`을 새 버전으로 고치고 다시 올리면, compose가 새 이미지를 받은 뒤 컨테이너를 바꿉니다("릴리스 이미지와 바이너리" 참고).

   ```bash
   docker compose up -d
   ```

   소스에서 빌드한다면 새 버전의 checkout에서 이미지를 빌드하고(`docker build --build-arg VERSION=v1.2.4 -t commandcenter:1.2.4 .`) `.env`의 `COMMANDCENTER_VERSION`을 그 태그(`1.2.4`)로 바꿔 올립니다. 이전 컨테이너는 진행 중인 요청을 마친 뒤 끝납니다("정상 종료" 참고). Helm 차트로 설치했다면 "차트 업그레이드"를 따릅니다.

   데이터 볼륨은 그대로 유지되고, 새 버전은 처음 시작할 때 `meta.db`를 필요한 만큼 새 구조로 바꿉니다. 구조 변경과 DB 구조 버전 기록은 한 트랜잭션으로 하므로, 도중에 중단되어도 `meta.db`는 이전 상태로 남고 다음 시작 때 다시 바꿉니다.
4. 시작 로그에 ERROR가 없는지 보고, 로그인·다운로드·업로드를 한 번씩 확인합니다. UI 세션은 재시작으로 끊기고 API 토큰은 유지됩니다.

**다운그레이드는 지원하지 않습니다.** 이 버전부터 `meta.db`에 DB 구조 버전(SQLite `PRAGMA user_version`, 지금은 1)을 기록하고, 자기보다 새 버전이 기록한 `meta.db`로는 시작하지 않습니다. 이때 시작 오류는 `/data/meta.db was written by a newer version of Command Center (database schema 2; this version supports up to 1) …`처럼 두 구조 버전을 알려 주며, `meta.db`는 건드리지 않습니다. 되돌려야 하면 업그레이드 전에 받은 백업을 이전 버전의 이미지와 함께 복원하세요("백업과 복원" 참고). 백업 뒤에 올라온 패키지와 바뀐 설정은 사라집니다.

이 검사가 없는 이전 버전(구조 버전을 기록하지 않음)은 새 버전이 쓴 데이터로도 경고 없이 시작해 잘못 동작합니다. 사용자를 `config.yaml`에서 읽던 버전으로 내리면 인증이 `config.yaml`의 해시로 돌아가 UI에서 바꾼 비밀번호가 통하지 않습니다. 이름 붙은 blob store를 모르는 버전으로 내리면 기본이 아닌 store에 둔 저장소는 기존 파일을 찾지 못하고, 새로 올린 파일은 default store(`<dataDir>/blobs`)에 씁니다. 비밀값 키를 정해 두었다면 암호화된 값을 그대로 비밀번호로 보내 원격·SMTP·LDAP 인증이 실패합니다. 그렇게 돌린 뒤 이 버전으로 다시 올렸다면, default store의 GC는 어느 저장소든 참조하는 blob을 지우지 않으므로 그동안 쓴 파일은 남아 있습니다("Blob Store" 참고). 서버를 멈추고 default store의 blob을 그 저장소의 store로 복사하면 다시 받을 수 있습니다. 내용 주소 방식이라 이름이 같으면 내용도 같으므로 덮어써도 무해하고, 그 store의 저장소가 참조하지 않는 사본은 다음 compact 작업이 지웁니다. 경로를 비워 만든 store라면 아래와 같이 합니다(절대 경로로 만든 store는 그 경로를 마운트해 바꿔 쓰세요).

```bash
docker compose stop
docker run --rm -u 65532:65532 -v commandcenter-data:/data busybox cp -a /data/blobs/sha256/. /data/blobstores/<store 이름>/blobs/sha256/
docker compose start
```

**사용자를 `config.yaml`에서 읽던 버전에서 올릴 때(설정 가져오기)**: 첫 오픈소스 릴리스(2026-10) 전의 빌드(repolite 포함)는 사용자와 역할을 시작할 때마다 `config.yaml`에서 읽었습니다. 지금 버전은 처음 시작할 때 `config.yaml`의 `users`, `roles`, `anonymousRead`, `baseURL`, `maxUploadMiB`를 DB로 **한 번** 가져옵니다(`repositories`와 같은 방식). 가져온 뒤에는 이 키들이 파일에 있어도 무시되며 시작할 때마다 경고가 한 줄 남습니다. 이 값들은 UI(사용자, 역할, Realm, 일반 설정)에서 관리합니다. `config.yaml`에는 `listen`, `dataDir`, `memoryLimitMiB`, `metricsFlushSeconds`(그리고 쓴다면 `secretKeyFile`)만 남기면 됩니다. 가져오기 전 `users`나 `roles` 항목이 잘못되어 있으면 시작이 실패하고, 가져온 뒤에 생긴 문제는 경고로만 남습니다. **업그레이드 전에** `config.yaml`의 `passwordHash`가 실제 비밀번호의 해시인지 확인하세요. 가져간 해시가 틀렸다면 파일을 고쳐도 반영되지 않으므로 UI의 사용자 관리나 `user reset-password`("비상 복구" 참고)로 비밀번호를 재설정해야 합니다. 기존 API 토큰과 `meta.db`는 그대로 쓰이고, 세션은 서버 재시작으로 끊깁니다. 예전 `write` 역할은 해당 저장소(또는 `*`)의 browse, read, add, edit, delete 전부로 바뀌며(예전 쓰기는 삭제를 포함했습니다), 로그인한 사용자의 읽기는 `authenticatedRead` 기본값(켜짐) 덕분에 예전과 같습니다. 가져온 뒤에는 `config.yaml`을 고쳐도 사용자·역할이 바뀌지 않으므로 UI를 쓰세요.

### 메모리 설정

- `memoryLimitMiB`(기본 80)는 Go 메모리 한도를 정합니다. 단, `GOMEMLIMIT` 환경변수가 있으면 그 값이 우선하며 compose 파일은 `GOMEMLIMIT: 80MiB`를 설정합니다. 한도를 바꾸려면 둘 중 실제로 쓰이는 쪽(compose 사용 시 `GOMEMLIMIT`)을 고치세요.
- compose의 `mem_limit: 128m`은 컨테이너 하드 한도입니다. 위 두 값보다 충분히 커야 합니다.
- 원격 다운로드는 동시에 32개까지만 진행합니다.
- 동시 연결은 **1024개**까지 받습니다. 자리가 차면 새 연결을 받기 위해, 요청 사이에 쉬고 있는(keep-alive) 연결이나 첫 요청의 헤더를 아직 다 보내지 않은 연결 가운데 그 상태로 가장 오래(1초 이상) 있던 것을 닫습니다. 요청을 처리 중인 연결은 닫지 않으며, 닫을 수 있는 연결이 없으면 새 연결은 커널 대기열(listen backlog)에서 기다립니다. keep-alive 유휴 연결은 자리가 남아 있어도 60초 뒤에 닫힙니다. 측정해 보면 연결 하나는 유휴, 헤더 수신 중, 본문 대기 중일 때 14~25 KiB에 받은 헤더 크기의 약 1.6배를 더 씁니다. 요청 헤더는 16 KiB까지만 받으므로(넘으면 431) 연결 하나는 많아야 약 40 KiB, 1024개면 약 40 MiB로 `GOMEMLIMIT`(80 MiB) 안에 머뭅니다. 고정값이며 설정으로 바꿀 수 없습니다. 훨씬 많은 클라이언트를 받아야 하면 앞단 리버스 프록시에서 연결을 모아 주세요.

### 업로드 크기 제한

| 대상 | 제한 | 초과 시 |
|---|---|---|
| 모든 포맷의 업로드(GET/HEAD 외) 본문 | `maxUploadMiB`(기본 1024 MiB) | 413 |
| Maven `maven-metadata.xml` 업로드 | 1 MiB | 413 |
| Maven `.pom` 업로드(classifier 없는 것) | 1 MiB | 413 |
| npm publish 문서(tarball 첨부 제외), unpublish 문서 | 10 MiB | 413 |
| npm tarball 검사: 압축을 푼 크기(publish 문서 하나의 새 tarball 합계) / 안의 `package.json` | 1 GiB / 1 MiB | 400 |
| PyPI 업로드 폼 | 사용하는 필드 각 4 KiB, 그 밖의 필드 합계 16 MiB, 필드 최대 256개 | 413 |
| PyPI 파일 검사 | zip(wheel, zip sdist) 항목 10,000개, central directory 2 MiB 이하(서버가 직접 센 값). `.tar.gz` sdist는 `PKG-INFO`가 압축을 푼 처음 512 MiB 안 | 400 |
| Go `.zip` / `.mod` / `.info` | 500 MiB / 16 MiB / 1 MiB | 413 |
| Go zip 구조 | 항목 50000개, 중앙 디렉터리 4 MiB 이하(서버가 직접 센 값) | 400 |
| Go zip 내용 | 압축 해제 합계 500 MiB(실제로 읽은 바이트), 루트 `LICENSE`·`go.mod` 각 16 MiB | 400 |
| Cargo crate / publish 메타데이터 | 50 MiB / 1 MiB | 413 |
| Cargo crate 안의 `Cargo.toml` | 1 MiB | 400 |

## 제한 사항

- 웹 UI로 저장소 만들기·편집·삭제, 사용자·역할·Realm(LDAP)·Blob Store·일반 설정·이메일 관리, Maven, npm, PyPI 업로드를 할 수 있습니다. SAML, Go·Cargo 업로드 화면은 없습니다. 저장소 정의는 DB에 있고 `config.yaml`의 `repositories:`는 첫 시작에만 쓰입니다. Docker proxy·group, S3 등 원격 blob 저장소, 포맷 고유의 검색(`npm search` 포함)도 없습니다.
- **인스턴스는 하나만**: 잠금이 모두 프로세스 안에 있어 같은 데이터로 여러 대를 띄울 수 없습니다. 이중화와 수평 확장은 없습니다("저장 공간과 인스턴스" 참고).
- HTTPS를 직접 받지 않습니다. 리버스 프록시에서 TLS를 끝내세요("TLS와 리버스 프록시" 참고). 다운그레이드는 지원하지 않습니다("업그레이드" 참고).
- **의도적으로 넣지 않은 것**: 저장소·사용자 **할당량(quota)**, 에디션(Pro/OSS) 구분, **SMTP의 PLAIN 외 인증**(LOGIN, CRAM-MD5, OAuth), TLS 검증 생략 옵션, **AD 확장 매칭 필터**(`:1.2.840…:=`), LDAP 부분 일치·비교 필터, 중첩 그룹 해석, LDAP 사용자 자동 삭제·토큰 자동 폐기, S3 같은 원격 blob store(경로는 로컬 디렉터리만), 이미 만든 저장소의 blob store 이동.
- PyPI는 PEP 658 메타데이터 파일과 `/pypi/<프로젝트>/json`을 제공하지 않습니다.
- Go 체크섬 DB(`sumdb/`)는 프록시하지 않습니다. 사내 모듈에는 `GONOSUMDB`를 쓰세요.
- Go zip 검사는 go 명령의 규칙을 전부 옮기지는 않았습니다. 하위 디렉터리의 `go.mod`, 루트의 `GO.MOD`처럼 대소문자가 다른 `go.mod`, 파일 이름의 `*`·`?`·`"`·`'`·`<`·`>`·`|`·백틱과 제어 문자, `.`으로 끝나는 이름, 다른 항목의 상위 디렉터리와 이름이 같은 파일(`a`와 `a/b.go`)은 받아들이지만, go 명령은 이런 zip을 내려받을 때 거부합니다. 이런 zip은 올리지 마세요.
- 삭제는 hosted 저장소에서만 됩니다(`npm unpublish`와 Go `DELETE`는 group으로 보내면 `writeMember`에서 지웁니다). proxy의 캐시는 Cleanup 정책으로만 지웁니다. twine과 cargo에는 삭제 명령이 없으므로 PyPI와 Cargo는 웹 UI나 API로 지우고, 패키지 문서가 10 MiB를 넘는 npm 패키지는 버전 하나를 지우는 `npm unpublish`가 413이므로 역시 웹 UI나 API로 지웁니다("컴포넌트 삭제" 참고).
- 같은 파일·버전을 다시 올릴 때의 응답:

  | 포맷 | 응답 |
  |---|---|
  | Maven (`allow_once`) | 400 |
  | npm | 403 |
  | PyPI | 400 `File already exists.` |
  | Go | 409 (`writePolicy: allow`이면 허용) |
  | Cargo | 409 |

  Cargo는 `-`/`_`와 대소문자만 다른 이름, 빌드 메타데이터(`+…`)만 다른 버전도 같은 것으로 보고 409로 거부합니다.
- **group과 의존성 혼동**: group은 멤버를 순서대로 조회합니다. Maven과 Go는 `latest`를 멤버 전체 중 가장 새로운 버전으로 고르므로, hosted 멤버를 앞에 두어도 proxy 쪽에 더 높은 버전이 있으면 그것이 `latest`가 됩니다. npm·PyPI·Cargo group은 그 패키지(프로젝트, crate)를 처음 가진 멤버 하나만으로 답하므로, hosted를 proxy보다 앞에 두면 같은 이름의 공개 버전이 섞이지 않습니다. npm과 PyPI는 앞선 멤버가 Offline이거나 오류를 내면 503·5xx로 실패하고, tarball·파일 URL을 직접 요청해도 그 멤버의 것만 줍니다. Cargo group은 Offline 멤버를 건너뛰고, crate 파일 다운로드 URL은 멤버 순서대로 찾습니다. Maven·Go·Cargo에서 사내 패키지 이름을 공개 저장소에서 선점당하지 않으려면 사내 전용 group을 따로 두거나 proxy 멤버를 넣지 마세요.
- Maven 메타데이터를 가진 멤버가 하나뿐이면 병합하지 않고 그 문서를 그대로 제공합니다. 다만 Maven 그룹에 이미 병합해 저장한 문서가 있으면 내용이 달라졌을 때 새로 저장하여 `Last-Modified`가 앞으로 움직입니다. npm 그룹은 packument를 병합하지 않고 처음 가진 멤버의 문서로 답하므로, 그 멤버가 바뀌면(예: hosted에서 마지막 버전을 지워 proxy가 답하게 되면) `Last-Modified`가 뒤로 갈 수 있습니다. 이전 버전이 그룹에 저장해 둔 병합 packument는 읽지 않습니다.
- 405 응답은 `Allow` 헤더로 그 경로가 실제로 받는 메서드를 알려 줍니다(예: npm dist-tag 경로 `/-/package/{pkg}/dist-tags/{tag}`는 `PUT, DELETE`, proxy 저장소의 쓰기 경로는 `GET, HEAD`). 받는 메서드가 없는 경로(proxy와 `writeMember`가 없는 group의 npm `-rev`, proxy의 Cargo publish·PyPI 업로드 경로)는 빈 `Allow`를 보냅니다. hosted(와 `writeMember`가 있는 group)의 npm `{pkg}/-rev/{rev}`는 `PUT, DELETE`, tarball의 `-rev`는 `DELETE`, Go `.mod`·`.info`·`.zip`은 `GET, HEAD, PUT, DELETE`를 알립니다.
- proxy가 받은 파일의 체크섬이 원격 인덱스와 다르면 첫 요청은 502이고, 이후 `negativeCacheTTL` 동안은 404입니다. 이 404는 그 사이 원격 인덱스가 바뀌어도 만료될 때까지 유지됩니다.

## 문제 해결

- **`baseURL` 불일치**: npm/cargo가 "다운로드 실패", 엉뚱한 호스트로 접속, `ECONNREFUSED`를 내면 응답 안의 URL이 클라이언트가 접속하는 주소와 다른 것입니다. `baseURL`을 클라이언트가 쓰는 주소(스킴, 호스트, 포트)와 똑같이 맞추고 `docker compose restart` 하세요. pip는 PyPI 파일 링크가 호스트 없는 경로라서 해당하지 않습니다(`baseURL`의 경로 부분만 맞으면 됩니다).
- **npm 401**: `~/.npmrc`의 토큰이 서버에 없으면(예: 볼륨을 다시 만든 뒤) `anonymousRead`가 켜져 있어도 모든 요청이 401입니다. `npm login`을 다시 하거나 `_authToken` 줄을 지우세요.
- **`meta.db was written by a newer version of Command Center`**: 더 새 버전이 쓴 `meta.db`로 이전 버전을 띄웠습니다. 새 버전으로 다시 시작하거나, 업그레이드 전에 받은 백업을 지금 버전과 함께 복원하세요("업그레이드" 참고).
- **`it is encrypted, and no secret key is set` / `it was encrypted with another key`**: 비밀값 키가 없거나 다릅니다. 암호화할 때 쓴 키를 `COMMANDCENTER_SECRET_KEY`나 `secretKeyFile`로 주세요. `secret-key rotate`로 키를 바꿨다면 새 키이고, 교체 전에 받은 백업을 복원했다면 이전 키입니다("저장된 비밀값 암호화" 참고).
- **`508 proxy loop detected`**: proxy 저장소의 `remote`가 결국 같은 Command Center 인스턴스를 가리키면(예: 자기 자신의 그룹) 요청이 돌고 돌지 않도록 서버가 508로 거절합니다. 요청마다 `X-Command-Center-Hop` 헤더에 인스턴스 ID를 덧붙여 알아내므로, 서로 다른 두 인스턴스가 이어지는 것(A가 B를 proxy)은 막지 않습니다. `remote`를 고치세요.
- **`431 Request Header Fields Too Large`**: 요청 헤더가 16 KiB를 넘었습니다. 브라우저라면 같은 사이트(상위 도메인 포함)에 쌓인 쿠키가 큰 경우가 많으니 쿠키를 지워 보세요.
- **웹 UI의 저장·업로드가 403 `cross-origin request detected…`**: 브라우저의 `Origin`이 서버가 받은 `Host`와도, `baseURL`의 출처(스킴, 호스트, 포트)와도 다릅니다. 보통 `Host`를 바꾸는 프록시 뒤에서 평문 HTTP로 접속할 때이니, `baseURL`을 브라우저 주소창의 주소와 똑같이 맞추세요("TLS와 리버스 프록시" 참고). `baseURL`에 기본 포트(`:80`, `:443`)를 적으면 브라우저의 `Origin`과 맞지 않으니 빼세요.
- **npm·PyPI 그룹이 `503 group member "…" is offline`**: 그 멤버가 Offline입니다. 요청한 패키지를 그 멤버보다 앞의 멤버가 갖고 있지 않으면 그룹은 다음 멤버로 넘어가지 않습니다(사내 패키지 대신 같은 이름의 공개 패키지가 설치되는 것을 막기 위해서입니다). 멤버를 Online으로 돌리거나 group의 멤버 목록에서 빼세요.
- **proxy가 502이고 로그에 `address refused by the remote egress policy`**: 원격, 원격의 리다이렉트, 또는 원격 인덱스가 준 파일 URL이 허용되지 않은 내부 주소로 향했거나, 호스트가 `10.1`, `167772161`처럼 네 부분의 십진수가 아닌 숫자 표기이거나 ASCII가 아닌 이름입니다("저장소 키"의 "원격 접속 제한" 참고). 사내 미러를 쓰려면 `remote`를 그 미러의 사내 주소(사설 IP로 해석되는 이름)로 지정하고, IP 주소는 `10.0.0.1`처럼 네 부분으로, 국제화 도메인은 퓨니코드(`xn--…`)로 적으세요.
- **로그에 `stopped after 5 redirects`**: 원격이 리다이렉트를 5번 넘게 이어 갔습니다. `remote`를 최종 주소로 바꾸세요.
- **`remoteAuth`를 쓰는 proxy의 로그에 `remote status 401`(또는 403)**: 클라이언트는 502(만료된 캐시가 있으면 그 캐시)를 받습니다. 자격 증명이 맞다면, 원격이 다른 포트, `http`, 하위 도메인으로 리다이렉트해서 그 요청에 자격 증명을 보내지 않은 것일 수 있습니다(`remoteAuth` 참고. 같은 호스트의 `http`에서 `https`로 옮기는 리다이렉트는 자격 증명을 유지합니다). TLS를 끝내는 앞단 뒤의 원격이 `http://…:8081/` 같은 주소로 리다이렉트하는 경우가 흔하니, 원격이 자기 `https` 주소로 리다이렉트하게 고치세요(앞단이 `X-Forwarded-Proto`를 넘기게 하는 등). 최종 주소가 `https`라면 `remote`를 그 주소로 바꿔도 됩니다.
- **Kubernetes에서 Pod가 `CreateContainerConfigError`**: `secretKey.existingSecret`이 가리키는 Secret이나 그 안의 키(`secretKey.key`)가 없습니다. Secret을 만들면 Pod가 곧 시작합니다("Helm 차트로 설치" 참고).
- **`config.yaml`이 디렉터리**: `config.yaml` 없이 `docker compose up`을 하면 Docker가 같은 이름의 디렉터리를 만듭니다. `docker compose down`, 디렉터리 삭제, 파일 복사 후 다시 실행하세요.
- **Windows 경로**: Git Bash에서 `docker run -v`에 `/c/...` 같은 경로를 쓰면 경로가 변환되어 마운트가 어긋날 수 있습니다. `MSYS_NO_PATHCONV=1`을 설정하고 `$(pwd -W)`(또는 PowerShell의 `${PWD}`)를 쓰세요. 데이터는 바인드 마운트보다 named volume(compose 기본값)이 SQLite 잠금과 I/O에 안정적입니다.
- **시작 실패 / 동작 이상**: 먼저 `docker logs commandcenter`(또는 `docker compose logs`)를 확인하세요. 설정 오류는 시작할 때 항목별로 출력됩니다.
- **로그인이 `429 too many failed sign-ins`**: 그 이름으로 실패가 반복되어 잠시 거절된 것입니다("보호"의 로그인 시도 제한). `Retry-After`만큼 기다리거나 관리자가 비밀번호를 재설정하면 풀립니다. 예전 비밀번호를 저장한 빌드나 CI가 계속 재시도하고 있지 않은지 서버 로그의 `sign-in throttled`와 요청 로그로 확인하세요.
- **proxy 저장소가 비어 있음**: 컨테이너에서 원격 레지스트리로 HTTPS 접속이 되어야 합니다. 사내 프록시가 있으면 compose의 `HTTPS_PROXY` 환경변수를 설정하고, 프록시를 거치지 않을 사내 호스트는 `NO_PROXY`에 넣으세요.

## 개발

로컬에 Go를 설치하지 않고 컨테이너(golang:1.26)에서 테스트합니다.

main에 push하거나 PR을 열면 GitHub Actions(`.github/workflows/ci.yml`)가 gofmt, `go vet`, `go test`(배포 바이너리처럼 CGO 끔), `go test -race`, govulncheck, UI 스크립트 구문 검사, 이미지 빌드(linux/amd64·arm64), Helm 차트와 참조 매니페스트 검사(`helm lint`, `helm template`, kubeconform)를 실행합니다. `ci.yml`의 `go-version`이 Dockerfile의 `golang` 태그(현재 1.26.10)와 다르거나, `THIRD_PARTY_LICENSES.txt`가 바이너리에 링크되는 모듈·Go 버전과 맞지 않으면(`scripts/third-party-licenses.sh --check`) CI가 실패합니다. Dependabot이 Go 모듈, 베이스 이미지, Actions 업데이트를 매주 PR로 올리는데, 링크되는 모듈이나 `golang` 태그를 바꾸는 PR은 이 검사에서 실패합니다. 그 PR 브랜치에서 `bash scripts/third-party-licenses.sh`로 `THIRD_PARTY_LICENSES.txt`를 다시 만들어 커밋하고(스크립트는 Dockerfile의 `golang` 이미지에서 돌므로 Docker만 있으면 됩니다), `golang` 태그가 바뀌었으면 `ci.yml`의 `go-version`도 같은 버전으로 올리세요.

릴리스는 태그를 푸시하면 됩니다(`git tag v1.2.3 && git push origin v1.2.3`). 결과물은 "릴리스 이미지와 바이너리"를 보세요. 여러 버전을 함께 낼 때(예: `v1.3.0`과 유지보수 릴리스 `v1.2.5`)는 태그를 한 번에 푸시하세요(`git push origin v1.2.5 v1.3.0`). 각 실행은 시작할 때 본 태그로 움직이는 태그를 정하므로, 낮은 버전의 실행이 높은 버전의 태그를 보지 못하면 `latest`가 낮은 버전으로 돌아갈 수 있습니다.

```bash
scripts/go.sh test ./...      # 단위·통합 테스트
scripts/go.sh vet ./...

test/e2e.sh                   # 스모크 테스트(포맷 5개, UI, LDAP, 메일)와 부하 테스트를 모두 실행하고 요약 출력

test/smoke/maven-smoke.sh     # 실제 클라이언트로 포맷별 E2E
test/smoke/npm-smoke.sh
test/smoke/pypi-smoke.sh
test/smoke/go-smoke.sh
test/smoke/cargo-smoke.sh

test/load/load.sh             # 128 MiB 컨테이너 메모리 부하 테스트
```

기여 절차와 코드 규칙은 [CONTRIBUTING.md](../CONTRIBUTING.md)에 있습니다.
