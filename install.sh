#!/bin/sh
# webdesk installer: installs git, curl and Docker if they're missing, fetches
# webdesk and starts it on http://127.0.0.1:8080.
#
#   curl -fsSL https://raw.githubusercontent.com/rmegi/webdesk/main/install.sh | sh
#
# Add the test machine (SSH + XFCE) as something to connect to:
#
#   curl -fsSL .../install.sh | sh -s -- --with-target
#
# Run it again to update. Linux only; macOS and Windows need Docker Desktop.
#
#   WEBDESK_DIR     where the code goes (default ~/webdesk)
#   WEBDESK_REPO    git URL to clone (default github.com/rmegi/webdesk)
#   WEBDESK_BRANCH  branch to run (default main)
#
# Everything lives in main(), called on the last line: piped into sh, nothing
# runs until the whole script has arrived, and commands that read stdin can't
# swallow the rest of it.

set -eu

REPO=${WEBDESK_REPO:-https://github.com/rmegi/webdesk.git}
BRANCH=${WEBDESK_BRANCH:-main}
DIR=${WEBDESK_DIR:-$HOME/webdesk}
URL=http://127.0.0.1:8080

say() { printf '\033[1m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33mwarning:\033[0m %s\n' "$*" >&2; }
die() { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# ---------- root ----------

as_root() {
  if [ "$(id -u)" -eq 0 ]; then
    "$@"
  elif have sudo; then
    sudo "$@"
  else
    die "this needs root for: $*. Install sudo or run as root."
  fi
}

# ---------- packages ----------

# Sets DISTRO, LIKE and PKG from /etc/os-release and what's on PATH.
detect_system() {
  case $(uname -s) in
    Linux) ;;
    Darwin) die "macOS: install Docker Desktop (https://docs.docker.com/desktop/), start it, then run: git clone $REPO && cd webdesk && docker compose up -d --build" ;;
    *) die "unsupported system $(uname -s). On Windows, run this inside WSL." ;;
  esac
  [ -r /etc/os-release ] || die "can't read /etc/os-release to tell which Linux this is."
  # shellcheck disable=SC1091
  . /etc/os-release
  DISTRO=${ID:-unknown}
  LIKE=${ID_LIKE:-}
  if have apt-get; then PKG=apt
  elif have dnf; then PKG=dnf
  elif have yum; then PKG=yum
  elif have zypper; then PKG=zypper
  elif have pacman; then PKG=pacman
  else PKG=none
  fi
}

APT_UPDATED=
pkg_install() {
  case $PKG in
    apt)
      if [ -z "$APT_UPDATED" ]; then
        as_root apt-get update -q </dev/null
        APT_UPDATED=1
      fi
      as_root env DEBIAN_FRONTEND=noninteractive apt-get install -y -q "$@" </dev/null ;;
    dnf) as_root dnf install -y -q "$@" </dev/null ;;
    yum) as_root yum install -y -q "$@" </dev/null ;;
    zypper) as_root zypper --non-interactive install "$@" </dev/null ;;
    pacman) as_root pacman -S --needed --noconfirm "$@" </dev/null ;;
    *) die "no package manager found to install: $*" ;;
  esac
}

# ssh-keygen's package goes by several names.
ssh_package() {
  case $PKG in
    apt) echo openssh-client ;;
    dnf | yum) echo openssh-clients ;;
    *) echo openssh ;;
  esac
}

install_tools() {
  missing=
  have git || missing="$missing git"
  have curl || missing="$missing curl"
  have ssh-keygen || missing="$missing $(ssh_package)"
  [ -z "$missing" ] && return
  say "Installing$missing"
  # shellcheck disable=SC2086
  pkg_install $missing ca-certificates
}

# ---------- docker ----------

is_like() {
  case " $DISTRO $LIKE " in *" $1 "*) return 0 ;; esac
  return 1
}

# Docker's own apt repository, for Ubuntu and Debian derivatives that
# get.docker.com turns away (Mint, Pop!_OS, ...).
install_docker_apt_repo() {
  if [ -n "${UBUNTU_CODENAME:-}" ]; then
    base=ubuntu codename=$UBUNTU_CODENAME
  elif [ -n "${DEBIAN_CODENAME:-}" ]; then
    base=debian codename=$DEBIAN_CODENAME
  else
    die "can't tell which Ubuntu or Debian release $DISTRO is based on. Install Docker yourself (https://docs.docker.com/engine/install/) and run this again."
  fi
  pkg_install ca-certificates curl
  as_root install -m 0755 -d /etc/apt/keyrings
  curl -fsSL "https://download.docker.com/linux/$base/gpg" | as_root tee /etc/apt/keyrings/docker.asc >/dev/null
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/$base $codename stable" \
    | as_root tee /etc/apt/sources.list.d/docker.list >/dev/null
  APT_UPDATED=
  pkg_install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
}

# Docker's own rpm repository, for Rocky, Alma and other RHEL or Fedora
# derivatives.
install_docker_rpm_repo() {
  if is_like fedora && ! is_like rhel && ! is_like centos; then base=fedora; else base=centos; fi
  curl -fsSL "https://download.docker.com/linux/$base/docker-ce.repo" | as_root tee /etc/yum.repos.d/docker-ce.repo >/dev/null
  pkg_install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
}

install_docker() {
  say "Installing Docker"
  case $DISTRO in
    ubuntu | debian | raspbian | fedora | centos | rhel)
      # Docker's official script: adds their repository, installs Engine,
      # Buildx and Compose.
      curl -fsSL https://get.docker.com | as_root sh ;;
    *)
      if [ "$PKG" = apt ]; then install_docker_apt_repo
      elif [ "$PKG" = dnf ] || [ "$PKG" = yum ]; then install_docker_rpm_repo
      elif [ "$PKG" = pacman ]; then pkg_install docker docker-compose docker-buildx
      elif [ "$PKG" = zypper ]; then pkg_install docker docker-compose docker-buildx
      else die "don't know how to install Docker on $DISTRO. Install it yourself (https://docs.docker.com/engine/install/) and run this again."
      fi ;;
  esac
}

# Docker is there but "docker compose" isn't, e.g. an old distro package.
install_compose() {
  say "Installing Docker Compose"
  case $PKG in
    apt) pkg_install docker-compose-plugin 2>/dev/null || pkg_install docker-compose-v2 2>/dev/null || true ;;
    dnf | yum) pkg_install docker-compose-plugin 2>/dev/null || true ;;
    pacman | zypper) pkg_install docker-compose 2>/dev/null || true ;;
  esac
  as_root docker compose version >/dev/null 2>&1 && return
  # No package for it: Docker's release binary, as a CLI plugin.
  as_root mkdir -p /usr/local/lib/docker/cli-plugins
  as_root curl -fsSL -o /usr/local/lib/docker/cli-plugins/docker-compose \
    "https://github.com/docker/compose/releases/latest/download/docker-compose-linux-$(uname -m)"
  as_root chmod +x /usr/local/lib/docker/cli-plugins/docker-compose
}

start_docker() {
  as_root docker info >/dev/null 2>&1 && return
  say "Starting Docker"
  if [ -d /run/systemd/system ]; then
    as_root systemctl enable --now docker
  elif have service; then
    as_root service docker start
  else
    # No init system to ask, e.g. WSL without systemd.
    as_root sh -c 'nohup dockerd >/var/log/dockerd.log 2>&1 &'
  fi
  i=0
  until as_root docker info >/dev/null 2>&1; do
    i=$((i + 1))
    [ "$i" -le 30 ] || die "Docker didn't start. Check: sudo docker info"
    sleep 1
  done
}

# Sets DOCKER to the command that reaches the daemon for this run.
setup_docker() {
  if ! have docker; then
    install_docker
  fi
  start_docker
  if ! as_root docker compose version >/dev/null 2>&1; then
    install_compose
    as_root docker compose version >/dev/null 2>&1 || die "couldn't install Docker Compose."
  fi

  if docker info >/dev/null 2>&1; then
    DOCKER=docker
    return
  fi
  DOCKER="as_root docker"
  [ "$(id -u)" -eq 0 ] && return
  # Next time, without sudo. Group membership only counts from the next login,
  # so this run keeps using sudo.
  user=$(id -un)
  members=$(getent group docker | cut -d: -f4 | tr ',' ' ')
  if ! printf ' %s ' "$members" | grep -q " $user "; then
    say "Adding $user to the docker group (takes effect at your next login)"
    getent group docker >/dev/null || as_root groupadd docker
    as_root usermod -aG docker "$user"
  fi
}

# ---------- webdesk ----------

fetch_code() {
  if [ -d "$DIR/.git" ]; then
    say "Updating $DIR"
    git -C "$DIR" fetch -q origin "$BRANCH"
    git -C "$DIR" checkout -q "$BRANCH"
    git -C "$DIR" merge -q --ff-only "origin/$BRANCH" \
      || die "$DIR has local changes that conflict with the update. Commit or stash them, then run this again."
  elif [ -e "$DIR" ]; then
    die "$DIR already exists and isn't a webdesk checkout. Set WEBDESK_DIR to install somewhere else."
  else
    say "Downloading webdesk into $DIR"
    git clone -q --branch "$BRANCH" "$REPO" "$DIR"
  fi
}

# compose mounts test/ssh into the server. If it's missing Docker creates it
# owned by root, and the test key can't be written there afterwards.
prepare_test_key() {
  keys=$DIR/test/ssh
  if [ -d "$keys" ] && [ ! -w "$keys" ]; then
    as_root chown "$(id -u):$(id -g)" "$keys"
  fi
  mkdir -p "$keys"
  if [ ! -f "$keys/id_ed25519" ]; then
    ssh-keygen -q -t ed25519 -N '' -C webdesk-test -f "$keys/id_ed25519"
  fi
}

start_app() {
  say "Building and starting webdesk (the first build takes a few minutes)"
  cd "$DIR"
  if [ "$WITH_TARGET" = 1 ]; then
    $DOCKER compose --profile target up -d --build
  else
    $DOCKER compose up -d --build
  fi
  i=0
  until curl -fsS -o /dev/null "$URL"; do
    i=$((i + 1))
    [ "$i" -le 60 ] || die "webdesk started but $URL isn't answering. Logs: cd $DIR && $(docker_hint) compose logs"
    sleep 1
  done
}

# How to type the docker command yourself, for the hints printed at the end.
docker_hint() {
  if [ "$DOCKER" = docker ] || [ "$(id -u)" -eq 0 ]; then echo docker; else echo sudo docker; fi
}

usage() {
  cat <<EOF
Usage: install.sh [--with-target]

Installs git, curl and Docker if missing, downloads webdesk into $DIR
and starts it on $URL. Run again to update.

  --with-target  also start the test machine (SSH + XFCE desktop)
EOF
}

main() {
  WITH_TARGET=0
  for arg in "$@"; do
    case $arg in
      --with-target) WITH_TARGET=1 ;;
      -h | --help) usage; exit 0 ;;
      *) usage >&2; die "unknown option: $arg" ;;
    esac
  done

  detect_system
  install_tools
  setup_docker
  fetch_code
  prepare_test_key
  start_app

  echo
  say "webdesk is running: $URL"
  if [ "$WITH_TARGET" = 1 ]; then
    echo "    Test machine: host target, port 22, user desk, password desk"
  fi
  echo "    Stop it:  cd $DIR && $(docker_hint) compose --profile target down"
  echo "    Update:   run this installer again"
  echo
  echo "    The page has no login of its own and only listens on 127.0.0.1."
  echo "    Read \"Before you expose it\" in the README before changing that."
}

main "$@"
