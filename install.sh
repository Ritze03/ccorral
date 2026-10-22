#!/bin/sh
# ./install.sh --install | --remove [--yes]
# Contract used by ritz-get. ccorral does the real work (ccorral install /
# ccorral uninstall); this only builds it first.
set -eu

usage() {
    echo "usage: $0 --install | --remove [--yes]" >&2
    exit 2
}

action=
yes=
for arg in "$@"; do
    case $arg in
        --install|--remove)
            [ -z "$action" ] || usage
            action=${arg#--} ;;
        --yes|-y) yes=--yes ;;
        *) usage ;;
    esac
done
[ -n "$action" ] || usage

cd "$(dirname "$0")"

if [ "$action" = install ]; then
    if ! command -v go >/dev/null 2>&1; then
        id= like=
        if [ -r /etc/os-release ]; then
            # shellcheck disable=SC1091
            . /etc/os-release
            id=${ID:-} like=${ID_LIKE:-}
        fi
        case " $id $like " in
            *" arch "*)                 hint="sudo pacman -S go" ;;
            *" fedora "*)               hint="sudo dnf install golang" ;;
            *" debian "*|*" ubuntu "*)  hint="sudo apt install golang-go" ;;
            *)                          hint="install Go 1.21 or newer" ;;
        esac
        echo "ccorral: Go is required to build. Try: $hint" >&2
        exit 1
    fi
    # go.mod asks for Go 1.27, which most distros don't ship yet.
    # GOTOOLCHAIN=auto lets any Go >= 1.21 download the right toolchain.
    GOTOOLCHAIN=auto CGO_ENABLED=0 go build -o ccorral .
    # shellcheck disable=SC2086 # $yes is empty or one word
    ./ccorral install $yes
else
    if [ -x "$HOME/.local/bin/ccorral" ]; then
        bin=$HOME/.local/bin/ccorral
    elif [ -x ./ccorral ]; then
        bin=./ccorral
    else
        echo "ccorral is not installed"
        exit 0
    fi
    # shellcheck disable=SC2086
    "$bin" uninstall $yes
fi
