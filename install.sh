#!/bin/sh
set -eu

router_source_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
router_bin_dir="$HOME/.local/bin"
router_update_path=1
while [ "$#" -gt 0 ]; do
    case "$1" in
        --bin-dir)
            [ "$#" -ge 2 ] && [ -n "$2" ] || { printf '%s\n' '--bin-dir requires a directory' >&2; exit 1; }
            router_bin_dir=$2
            router_update_path=0
            shift 2
            ;;
        --no-path-update) router_update_path=0; shift ;;
        -h|--help)
            printf '%s\n' 'Usage: ./install.sh [--bin-dir DIRECTORY] [--no-path-update]' \
                'Builds router and links it into ~/.local/bin. Existing config and data stay in the checkout.' \
                'For the default directory, adds PATH to zsh/bash startup files if needed.'
            exit 0
            ;;
        *) printf 'Unknown option: %s\n' "$1" >&2; exit 1 ;;
    esac
done

command -v go >/dev/null 2>&1 || { printf '%s\n' 'Go 1.23 or newer is required to install router.' >&2; exit 1; }
mkdir -p "$router_bin_dir"
router_bin_dir=$(CDPATH= cd -- "$router_bin_dir" && pwd -P)
router_target="$router_source_dir/bin/router"
router_command="$router_bin_dir/router"
# Refuse to replace a different command, including a dangling foreign symlink.
if [ -e "$router_command" ] || [ -L "$router_command" ]; then
    if [ ! -L "$router_command" ] || [ "$(readlink "$router_command")" != "$router_target" ]; then
        printf 'A different command already exists at %s. Choose --bin-dir DIRECTORY.\n' "$router_command" >&2
        exit 1
    fi
fi

(cd "$router_source_dir" && go build -o bin/router ./cmd/router)
if [ ! -L "$router_command" ]; then
    ln -s "$router_target" "$router_command"
fi
printf 'Installed: %s\n' "$router_command"
printf 'Default configuration: %s/config.json\n' "$router_source_dir"
if [ ! -f "$router_source_dir/config.json" ]; then
    printf '%s\n' 'Create config.json from config.example.json and fill in your provider settings before starting.'
fi

case ":$PATH:" in
    *":$router_bin_dir:"*) ;;
    *)
        router_path_line='export PATH="$HOME/.local/bin:$PATH"'
        router_append_path() {
            if ! grep -Fq "$router_path_line" "$1" 2>/dev/null; then
                printf '\n# Claude Tier Router command\n%s\n' "$router_path_line" >> "$1"
                printf 'Added PATH entry to %s\n' "$1"
            fi
        }
        if [ "$router_update_path" -eq 1 ]; then
            case "${SHELL:-}" in
                */zsh) router_append_path "${ZDOTDIR:-$HOME}/.zshrc" ;;
                */bash)
                    router_append_path "$HOME/.bashrc"
                    # Bash login shells use the first existing profile in this list.
                    if [ -f "$HOME/.bash_profile" ]; then
                        router_append_path "$HOME/.bash_profile"
                    elif [ -f "$HOME/.bash_login" ]; then
                        router_append_path "$HOME/.bash_login"
                    else
                        router_append_path "$HOME/.profile"
                    fi
                    ;;
                *) printf 'Add %s to your shell PATH.\n' "$router_bin_dir" ;;
            esac
        else
            printf 'Add %s to your shell PATH.\n' "$router_bin_dir"
        fi
        printf '%s\n' 'Open a new terminal after updating PATH.'
        ;;
esac
printf '%s\n' 'From any work project: router claude' 'Validate settings without model calls: router check'
