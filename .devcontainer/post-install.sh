#!/bin/bash
set -euo pipefail

echo "===================================="
echo "Kubebuilder DevContainer Setup"
echo "===================================="

# Verify running as root (required for installing to /usr/share and /etc)
if [ "$(id -u)" -ne 0 ]; then
  echo "ERROR: This script must be run as root"
  exit 1
fi

BASH_COMPLETIONS_DIR="/usr/share/bash-completion/completions"

echo ""
echo "------------------------------------"
echo "Setting up bash completion..."
echo "------------------------------------"

# Enable bash-completion in root's .bashrc (devcontainer runs as root)
if ! grep -q "source /usr/share/bash-completion/bash_completion" ~/.bashrc 2>/dev/null; then
  echo 'source /usr/share/bash-completion/bash_completion' >> ~/.bashrc
  echo "Added bash-completion to .bashrc"
fi

echo ""
echo "------------------------------------"
echo "Installing mise + pinned dev tools..."
echo "------------------------------------"

# mise owns every tool version (see .mise/config.toml + .mise/mise.lock), so the
# devcontainer matches CI exactly. Install mise, then let it install the toolchain.
if ! command -v mise &> /dev/null; then
  # Pin the mise CLI version itself (the tools it then installs are pinned via
  # .mise/config.toml + .mise/mise.lock). The install script embeds checksums
  # for this exact version — see https://mise.jdx.dev/installing-mise.html.
  curl -fsSL https://mise.run | MISE_VERSION=v2026.7.0 sh
  export PATH="$HOME/.local/bin:$PATH"
fi

# Activate mise for interactive shells.
if ! grep -q 'mise activate bash' ~/.bashrc 2>/dev/null; then
  echo 'eval "$(mise activate bash)"' >> ~/.bashrc
  echo "Added mise activation to .bashrc"
fi

# Install the locked tool versions from the repo config.
mise trust --yes
mise install --locked

# Make the tools available for the rest of this script.
eval "$(mise activate bash --shims)"

echo ""
echo "------------------------------------"
echo "Generating bash completions..."
echo "------------------------------------"

for tool in kind kubebuilder kubectl helm; do
  if command -v "$tool" &> /dev/null; then
    if "$tool" completion bash > "${BASH_COMPLETIONS_DIR}/${tool}" 2>/dev/null; then
      echo "${tool} completion installed"
    else
      echo "WARNING: Failed to generate ${tool} completion"
    fi
  fi
done

if command -v docker &> /dev/null; then
  docker completion bash > "${BASH_COMPLETIONS_DIR}/docker" 2>/dev/null \
    && echo "docker completion installed" \
    || echo "WARNING: Failed to generate docker completion"
fi

echo ""
echo "------------------------------------"
echo "Configuring Docker environment..."
echo "------------------------------------"

echo "Waiting for Docker to be ready..."
for i in {1..30}; do
  if docker info >/dev/null 2>&1; then
    echo "Docker is ready"
    break
  fi
  if [ "$i" -eq 30 ]; then
    echo "WARNING: Docker not ready after 30s"
  fi
  sleep 1
done

# Create kind network (ignore if already exists)
if ! docker network inspect kind >/dev/null 2>&1; then
  docker network create kind >/dev/null 2>&1 \
    && echo "Created kind network" \
    || echo "WARNING: Failed to create kind network (may already exist)"
fi

echo ""
echo "------------------------------------"
echo "Verifying installations..."
echo "------------------------------------"
mise ls
docker --version

echo ""
echo "===================================="
echo "DevContainer ready!"
echo "===================================="
echo "Tools are managed by mise. Run tasks with 'mise run <task>' (see 'mise tasks')."
