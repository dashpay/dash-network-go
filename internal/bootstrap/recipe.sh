set -Eeuo pipefail
umask 077
mode=$1
owner=$2
plan=$3
instance=$4
arch=$5
shift 5
stage=prerequisites
trap 'printf "{\"error\":\"%s\"}\n" "$stage"' ERR
[[ "$mode" == probe || "$mode" == apply ]]
[[ $(id -u) == 0 ]]
command -v curl >/dev/null
command -v python3 >/dev/null
command -v flock >/dev/null
stage=os-check
. /etc/os-release
[[ "$ID" == ubuntu && "$VERSION_ID" == 24.04 ]]
[[ $(dpkg --print-architecture) == "$arch" ]]
stage=instance-identity
token=$(curl --noproxy '*' -fsS --connect-timeout 3 --max-time 5 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' http://169.254.169.254/latest/api/token)
actual=$(curl --noproxy '*' -fsS --connect-timeout 3 --max-time 5 -H "X-aws-ec2-metadata-token: $token" http://169.254.169.254/latest/meta-data/instance-id)
[[ "$actual" == "$instance" ]]
unset token
stage=cloud-init
if command -v cloud-init >/dev/null; then
    cloud-init status --format json | python3 -c 'import json,sys; assert json.load(sys.stdin)["status"] == "done"'
fi
stage=host-lock
lock=/run/dashnet-bootstrap.lock
if [[ "$mode" == apply ]]; then
    [[ ! -L "$lock" ]]
    exec 9>"$lock"
    flock -n 9
elif [[ -e "$lock" ]]; then
    [[ ! -L "$lock" ]]
    exec 9<"$lock"
    flock -n 9
fi
root=/var/lib/dashnet
stage=host-ownership
[[ ! -L "$root" ]]
if [[ -e "$root" ]]; then
    [[ -d "$root" && $(stat -c '%u:%a' "$root") == 0:700 ]]
    # An interrupted mkdir before the owner write is safe only if still empty.
    if [[ ! -e "$root/owner" ]]; then
        [[ -z $(ls -A "$root") ]]
    else
        [[ -f "$root/owner" && ! -L "$root/owner" && $(cat "$root/owner") == "$owner" ]]
    fi
fi
for file in bootstrap-plan ready releases data docker-config; do
    [[ ! -L "$root/$file" ]]
done
if [[ -e "$root/bootstrap-plan" ]]; then
    [[ $(cat "$root/bootstrap-plan") == "$plan" ]]
fi
# Do not operate an existing fleet through the fresh-host bootstrap command.
stage=existing-containers
if [[ -d /var/lib/docker/containers ]]; then
    [[ -z $(ls -A /var/lib/docker/containers) ]]
fi
docker() { command docker --host unix:///var/run/docker.sock --config "$root/docker-config" "$@"; }
if command -v docker >/dev/null && docker info >/dev/null 2>&1; then
    [[ -z $(docker ps -aq) ]]
fi
if [[ "$mode" == apply ]]; then
    stage=host-ownership
    install -d -m 0700 "$root"
    # Never replace an existing ownership marker. A partial marker fails closed.
    if [[ ! -e "$root/owner" ]]; then
        owner_tmp=$(mktemp /run/dashnet-owner.XXXXXX)
        printf '%s\n' "$owner" >"$owner_tmp"
        cp --no-clobber "$owner_tmp" "$root/owner"
        rm "$owner_tmp"
    fi
    [[ $(cat "$root/owner") == "$owner" ]]
    printf '%s\n' "$plan" >"$root/bootstrap-plan"
    install -d -m 0700 "$root/data" "$root/releases" "$root/docker-config"
    stage=runtime-install
    log=/var/log/dashnet-bootstrap.log
    [[ ! -L "$log" ]]
    touch "$log"
    chmod 0600 "$log"
    # Prebaked runtimes skip package work. Fresh Ubuntu uses signed distro repos;
    # no curl|sh installer, no Dashmate, no registry credentials or package upgrade.
    if ! command -v docker >/dev/null || ! docker compose version --short >/dev/null 2>&1; then
        DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=120 update >>"$log" 2>&1
        DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=120 install -y --no-install-recommends docker.io docker-compose-v2 >>"$log" 2>&1
    fi
    stage=runtime-start
    systemctl enable --now docker >>"$log" 2>&1
    [[ -z $(docker ps -aq) ]]
    stage=image-pull
    for image in "$@"; do
        docker pull --platform "linux/$arch" "$image" >>"$log" 2>&1
    done
fi
stage=runtime-verify
docker_version=''
compose_version=''
ready=false
if command -v docker >/dev/null && docker info >/dev/null 2>&1; then
    docker_version=$(docker version --format '{{.Server.Version}}')
    compose_version=$(docker compose version --short 2>/dev/null || true)
    if [[ -n "$compose_version" ]]; then
        ready=true
        for image in "$@"; do
            if ! docker image inspect "$image" 2>/dev/null | python3 -c 'import json,sys
# Docker canonicalizes Hub repositories differently from OCI resolver names.
def canonical(ref):
    for prefix in ("index.docker.io/", "docker.io/", "registry-1.docker.io/"):
        if ref.startswith(prefix):
            ref=ref[len(prefix):]
            break
    if "/" not in ref:
        ref="library/"+ref
    return ref
a=json.load(sys.stdin)
assert len(a)==1 and a[0]["Os"]=="linux" and a[0]["Architecture"]==sys.argv[1]
assert canonical(sys.argv[2]) in [canonical(d) for d in (a[0].get("RepoDigests") or [])]' "$arch" "$image"; then
                ready=false
            fi
        done
    fi
fi
if [[ "$mode" == apply ]]; then
    [[ "$ready" == true ]]
    stage=checkpoint
    printf '%s\n' "$@" >"$root/releases/$plan.images"
    # Completion is published last; a lost response is reconciled by probe.
    ready_tmp=$(mktemp "$root/.ready.XXXXXX")
    printf '%s\n' "$plan" >"$ready_tmp"
    mv -f "$ready_tmp" "$root/ready"
fi
if [[ ! -f "$root/ready" ]] || [[ $(cat "$root/ready") != "$plan" ]]; then
    ready=false
fi
stage=response
python3 -c 'import json,sys; print(json.dumps(dict(planId=sys.argv[1],instanceId=sys.argv[2],architecture=sys.argv[3],ready=sys.argv[4]=="true",dockerVersion=sys.argv[5],composeVersion=sys.argv[6])))' "$plan" "$instance" "$arch" "$ready" "$docker_version" "$compose_version"
