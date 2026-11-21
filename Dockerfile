# Build the guest-facing exeuntu helper.
FROM docker.io/library/golang:1.27.4 AS exeuntu-cli
ARG EXEUNTU_GIT_VERSION=unknown
WORKDIR /src/exeuntu-cli
COPY cli/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -mod=mod -tags osusergo,netgo \
        -ldflags "-X main.gitVersion=${EXEUNTU_GIT_VERSION} -extldflags=-static -s -w" \
        -o /out/exeuntu .

FROM ubuntu:24.04

# Switch from dash to bash by default.
SHELL ["/bin/bash", "-euxo", "pipefail", "-c"]


# Install packages with their documentation. The minimized ubuntu base ships
# none for its preinstalled packages; we deliberately do not run unminimize —
# restoring base man pages costs ~90 MB plus minutes of mandb indexing.
RUN sed -i 's|http://archive.ubuntu.com/ubuntu/|http://mirror://mirrors.ubuntu.com/mirrors.txt|' /etc/apt/sources.list && \
        rm -f /etc/dpkg/dpkg.cfg.d/excludes /etc/dpkg/dpkg.cfg.d/01_nodoc && \
	apt-get update && \
	# Pull in all available security/bugfix updates for packages already
	# in the base ubuntu:24.04 image. Without this we ship whatever was
	# current when Canonical last rebuilt the base layer, which can be
	# months behind (e.g. nginx Rift, CVE-2026-42945). The weekly cron
	# rebuild + no-cache will keep this fresh going forward.
	DEBIAN_FRONTEND=noninteractive apt-get -y \
		-o Dpkg::Options::=--force-confold \
		-o Dpkg::Options::=--force-confdef \
		dist-upgrade && \
	# Pre-configure debconf to avoid interactive prompts
	echo 'debconf debconf/frontend select Noninteractive' | debconf-set-selections && \
	# Pre-configure pbuilder to avoid mirror prompt
	echo 'pbuilder pbuilder/mirrorsite string http://archive.ubuntu.com/ubuntu' | debconf-set-selections && \
	DEBIAN_FRONTEND=noninteractive apt-get install -y \
		ca-certificates wget ripgrep \
		locales \
		git jq sqlite3 curl vim neovim lsof iproute2 less nginx \
		make python3-pip python-is-python3 tree net-tools file \
		pipx psmisc bsdmainutils sudo socat \
		openssh-server openssh-client \
		libcap2-bin unzip util-linux rsync \
		iputils-ping socat netcat-openbsd \
		ubuntu-server ubuntu-standard \
		mitmproxy \
		systemd systemd-sysv \
		atop btop iotop ncdu \
		git \
		bubblewrap \
		dbus-user-session \
		&& DEBIAN_FRONTEND=noninteractive apt-get purge -y \
			snapd debian-keyring \
			python3-botocore python-babel-localedata pocketsphinx-en-us \
			libllvm20 mesa-vulkan-drivers mesa-libgallium \
			libgl1-mesa-dri libglx-mesa0 mesa-vdpau-drivers mesa-va-drivers \
		&& apt-get autoremove --purge -y && \
		# locales-all carried every locale on earth; generate the ones actually
		# used (clients send LC_* over SSH — an ungenerated locale means
		# setlocale warnings and C fallback for collation).
		locale-gen en_US.UTF-8 en_GB.UTF-8 ru_RU.UTF-8 && \
		apt-get remove -y pollinate ubuntu-fan && \
		# Same-layer cleanup: package indexes and cached debs are dead weight
		# once the layer is committed.
		apt-get clean && rm -rf /var/lib/apt/lists/* && \
		# openssh-server generates host keys during package configuration.
		# Do not bake those per-image private keys into exeuntu.
		rm -f /etc/ssh/ssh_host_*_key /etc/ssh/ssh_host_*_key.pub && \
		# Allow non-root users to use ping without sudo by granting CAP_NET_RAW
		setcap cap_net_raw=+ep /usr/bin/ping

# Remove policy-rc.d so services can start normally (the base image includes this
# to prevent services from starting during build, but we run systemd at runtime)
RUN rm -f /usr/sbin/policy-rc.d

# No baked Go toolchain: it cost 282 MB and nothing in the image needs it.
# When a project needs Go, declare it in the dotfiles mise config and it lands
# through materialization like every other runtime.
#
# No baked browser, docker, build toolchain, media tools or tailscale either
# (~1 GB combined, all measured unused on real hosts): the dotfiles feature
# bundles (feat-browser, feat-docker, feat-build, feat-media, feat-tailscale)
# reinstall them on hosts that opt in, per host.

# Nothing in the image uses systemd-resolved; purging the base-image default
# keeps the lean footprint (upstream purges it to fix Tailscale DNS detection,
# which does not apply here since Tailscale arrives via dotfiles).
RUN apt-get purge -y systemd-resolved

COPY --from=exeuntu-cli /out/exeuntu /usr/local/bin/exeuntu

# mise is the one tool manager the image ships. Coding agents and runtimes
# (claude, codex, pi, gh, uv, node) are owned by the machine's dotfiles and
# materialized through it at boot; baking them here would duplicate every one
# of them once the dotfiles converge.
RUN curl -fsSL https://mise.run | env MISE_INSTALL_PATH=/usr/local/bin/mise sh && \
	/usr/local/bin/mise --version

# No baked DuckDB either: upstream installs the CLI plus httpfs/iceberg/avro
# extensions for its look integration, but per the tenet above the tool belongs
# to the dotfiles owner (a feat-* bundle on hosts that need it), not the image.

# Configure systemd
RUN rm /etc/systemd/system/multi-user.target.wants/console-setup.service \
		/etc/systemd/system/multi-user.target.wants/ModemManager.service \
		/etc/systemd/system/multi-user.target.wants/unattended-upgrades.* \
		/etc/systemd/system/multi-user.target.wants/ubuntu-advantage.service && \
	systemctl mask -- getty.target \
		fwupd.service \
		fwupd-refresh.service \
		fwupd-refresh.timer \
		systemd-random-seed.service \
		iscsid.socket \
		dm-event.socket \
		man-db.timer \
		update-notifier-download.timer \
		update-notifier-motd.timer \
		atop-rotate.timer \
		dpkg-db-backup.timer \
		e2scrub_all.timer \
		etc-resolv.conf.mount \
		etc-hosts.mount \
		etc-hostname.mount \
		-.mount \
		systemd-remount-fs.service \
		systemd-sysusers.service \
		systemd-update-done.service \
		systemd-update-utmp.service \
		systemd-journal-catalog-update.service \
		modprobe@.service \
		systemd-modules-load.service \
		systemd-udevd.service \
		systemd-udevd-control.service \
		systemd-udevd-kernel.service \
		systemd-udev-trigger.service \
		systemd-udev-settle.service \
		systemd-hwdb-update.service \
		ubuntu-fan.service \
		ldconfig.service \
		unattended-upgrades.service \
		lxd-installer.socket \
	        console-getty.service \
		keyboard-setup.service \
		systemd-ask-password-console.path \
		systemd-ask-password-wall.path \
		ssh.socket \
		ssh.service \
		plymouth.service \
		plymouth-start.service \
		plymouth-quit.service \
		plymouth-quit-wait.service \
		plymouth-read-write.service \
		plymouth-switch-root.service \
		plymouth-switch-root-initramfs.service \
		plymouth-halt.service \
		plymouth-reboot.service \
		plymouth-poweroff.service \
		plymouth-kexec.service \
		apt-daily-upgrade.timer \
		apt-daily.timer \
		plymouth-log.service && \
	# systemd-logind is disabled but not masked. It's involved in populating the XDG runtime dir sockets... somehow
	systemctl disable getty.target systemd-logind.service \
		nginx.service \
                   console-getty.service \
		   atop.service \
                   getty@.service \
		   motd-news.timer motd-news.service \
		    apport.service apport-autoreport.timer apport-autoreport.path apport-forward.socket \
		    udisks2.service \
		   ufw.service \
		   lvm2-lvmpolld.socket \
                   systemd-ask-password-wall.service \
                   systemd-ask-password-console.service \
                   systemd-machine-id-commit.service \
                   systemd-modules-load.service \
                   systemd-sysctl.service \
                   systemd-firstboot.service \
                   systemd-udevd.service \
                   systemd-udev-trigger.service \
                   systemd-udev-settle.service \
		   e2scrub_reap.service \
		   systemd-update-utmp.service \
		   atopacct.service \
		   sysstat.service \
                   systemd-hwdb-update.service \
		   multipathd.service && \
	mkdir -p /etc/systemd/system.conf.d && \
    		echo '[Manager]' > /etc/systemd/system.conf.d/container-overrides.conf && \
    		echo 'LogLevel=info' >> /etc/systemd/system.conf.d/container-overrides.conf && \
    		echo 'LogTarget=console' >> /etc/systemd/system.conf.d/container-overrides.conf && \
    		echo 'SystemCallArchitectures=native' >> /etc/systemd/system.conf.d/container-overrides.conf && \
    		echo 'DefaultOOMPolicy=continue' >> /etc/systemd/system.conf.d/container-overrides.conf && \
	mkdir -p /etc/systemd/journald.conf.d && \
		echo '[Journal]' > /etc/systemd/journald.conf.d/persistent.conf && \
		echo 'Storage=persistent' >> /etc/systemd/journald.conf.d/persistent.conf && \
	systemctl set-default multi-user.target

# Modify existing ubuntu user (UID 1000) to become exedev user
RUN usermod -l exedev -c "exe.dev user" ubuntu && \
	groupmod -n exedev ubuntu && \
	mv /home/ubuntu /home/exedev && \
	usermod -d /home/exedev exedev && \
	# systemd-journal + adm: read service journals (materialization log) without sudo.
	usermod -aG sudo,systemd-journal,adm exedev && \
	sed -i 's/^ubuntu:/exedev:/' /etc/subuid /etc/subgid && \
	echo 'exedev ALL=(ALL) NOPASSWD:ALL' >> /etc/sudoers && \
	echo 'Defaults:exedev verifypw=any' >> /etc/sudoers && \
	# Manually enable linger, this should autopopulate /run/user/1000
	mkdir -p /var/lib/systemd/linger && \
	touch /var/lib/systemd/linger/exedev

# Bake /etc/fstab so systemd-growfs@-.service resizes the root filesystem on
# first boot after the disk is grown.
RUN echo '/dev/vda / ext4 defaults,x-systemd.growfs 0 1' > /etc/fstab

# Stop systemd wiping /tmp at boot; that races non-systemd users of the system
# that also run at boot.
COPY tmpfiles-tmp.conf /etc/tmpfiles.d/tmp.conf

COPY --chmod=644 fstrim-timer.conf /etc/systemd/system/fstrim.timer.d/exe.conf

ENV EXEUNTU=1

# https://github.com/trfore/docker-ubuntu2404-systemd/blob/main/Dockerfile suggests the following
# might be useful?
# STOPSIGNAL SIGRTMIN+3


# No baked headless-shell: the dotfiles feat-browser bundle pulls the same
# chromedp artifact (crane export) on hosts that carry the feature, and
# symlinks /usr/local/bin/headless-shell.

RUN mkdir -p /home/exedev /home/exedev/.config/shelley && \
    chown exedev:exedev /home/exedev /home/exedev/.config /home/exedev/.config/shelley

USER exedev

WORKDIR /home/exedev

# Update PATH in .bashrc to include .local/bin and set XDG_RUNTIME_DIR for systemd user services
# XDG paths are not autopopulated despite the presense of libpam-systemd. Manually add them here.
# Nothing in the image installs into ~/.local/bin anymore, but both the PATH
# entry and the dotfiles installer expect the directory to exist.
RUN mkdir -p /home/exedev/.local/bin && \
    echo 'export PATH="$HOME/.local/bin:$PATH"' >> /home/exedev/.bashrc && \
    echo 'export XDG_RUNTIME_DIR="/run/user/$(id -u)"' >> /home/exedev/.bashrc && \
    echo 'export XDG_RUNTIME_DIR="/run/user/$(id -u)"' >> /home/exedev/.profile

# Configure git to use 'main' as default branch name
RUN git config --global init.defaultBranch main

# Switch back to root to install systemd service
USER root

# Disable Ubuntu's default MOTD (the sudo hint, etc.)
RUN rm -rf /etc/update-motd.d/* /etc/motd && touch /home/exedev/.hushlogin && chown exedev:exedev /home/exedev/.hushlogin

# Add custom MOTD to exedev's .bashrc (ignores .hushlogin - we handle that ourselves)
COPY motd-snippet.bash /tmp/motd-snippet.bash
RUN cat /tmp/motd-snippet.bash >> /home/exedev/.bashrc && rm /tmp/motd-snippet.bash

# Create systemd socket and service for Shelley (socket activation).
# The shelley binary itself is installed at vm creation.
COPY shelley.socket /etc/systemd/system/shelley.socket
COPY shelley.service /etc/systemd/system/shelley.service
COPY tmpfiles-shelley.conf /etc/tmpfiles.d/shelley.conf
RUN chmod 644 /etc/systemd/system/shelley.socket /etc/systemd/system/shelley.service && \
    systemctl enable shelley.socket

# Give git the identity exe.dev writes at VM creation (see
# https://exe.dev/docs/git-identity), as system config so that anything in
# ~/.gitconfig overrides it. git skips the include if the file is missing.
RUN git config --system include.path /exe.dev/etc/gitconfig

# Create systemd oneshot service for /exe.dev/setup script
COPY exe-setup.service /etc/systemd/system/exe-setup.service
RUN chmod 644 /etc/systemd/system/exe-setup.service && \
    systemctl enable exe-setup.service

# Materialize the machine's dotfiles from the repository discovered through
# reflection (a github integration whose comment is "dotfiles"): clone
# keylessly and run its unattended installer. Timer-driven so a long converge
# never delays boot reachability and a failed run retries on the next tick.
COPY exeuntu-materialize.service /etc/systemd/system/exeuntu-materialize.service
COPY exeuntu-materialize.timer /etc/systemd/system/exeuntu-materialize.timer
RUN chmod 644 /etc/systemd/system/exeuntu-materialize.service \
		/etc/systemd/system/exeuntu-materialize.timer && \
	systemctl enable exeuntu-materialize.service exeuntu-materialize.timer

# TODO(crawshaw/philip): This is called init so that exetini decides
# this wrapper script is an init, and exec's it rather than forking it.
# It would be better if you could indicate that via an env variable or something.
COPY init-wrapper.sh /usr/local/bin/init

# Create config directories for LLM agents.
RUN mkdir -p \
      /home/exedev/.claude \
      /home/exedev/.codex \
      /home/exedev/.pi \
      /home/exedev/.config/opencode && \
    chown -R exedev:exedev /home/exedev/.claude /home/exedev/.codex /home/exedev/.pi /home/exedev/.config/opencode

# mise owns the OpenCode binary. Ship its exe.dev integration in the system
# config directory so it is available without running the manual installer.
COPY cli/internal/agentupdate/opencode-plugin/opencode.json /etc/opencode/opencode.json
COPY cli/internal/agentupdate/opencode-plugin/exe-dev.js /etc/opencode/plugins/exe-dev.js
RUN chmod 644 /etc/opencode/opencode.json /etc/opencode/plugins/exe-dev.js

# Copy LLM agent instructions to Claude, Codex, OpenCode, Pi, and Shelley config directories
# Shelley and OpenCode use ~/.config/ (XDG convention).
# Source file is exeuntu-agents.md at the repo root: a name agents don't
# auto-discover, so host-side sessions working on this repo never load VM-only
# instructions by mistake.
COPY exeuntu-agents.md /home/exedev/.config/shelley/AGENTS.md
RUN chown exedev:exedev /home/exedev/.config/shelley/AGENTS.md && \
    ln -s /home/exedev/.config/shelley/AGENTS.md /home/exedev/.claude/CLAUDE.md && \
    ln -s /home/exedev/.config/shelley/AGENTS.md /home/exedev/.codex/AGENTS.md && \
    ln -s /home/exedev/.config/shelley/AGENTS.md /home/exedev/.config/opencode/AGENTS.md && \
    ln -s /home/exedev/.config/shelley/AGENTS.md /home/exedev/.pi/AGENTS.md

# Coding agents (claude, codex, opencode, pi) are not installed here: they
# arrive through the machine's mise-managed dotfiles (see
# exeuntu-materialize.timer below). `exeuntu update <agent>` remains available
# for manual installs.

# Install the pi exe.dev extension (LLM integration + environment context).
# The bundled public catalog supplies pricing and compatibility metadata only;
# reflection-discovered integrations supply every model and provider route.
COPY pi-extension/ /home/exedev/.pi/agent/extensions/exe-dev/
RUN curl -fsSL --retry 5 --retry-delay 2 --retry-all-errors --max-time 30 \
      https://exe.dev/llm-gateway-models.json \
      -o /home/exedev/.pi/agent/extensions/exe-dev/catalog.json && \
    jq -e '.schemaVersion | numbers' \
      /home/exedev/.pi/agent/extensions/exe-dev/catalog.json > /dev/null
RUN chown -R exedev:exedev /home/exedev/.pi/agent

# Pre-install fd at the path pi checks first (~/.pi/agent/bin/fd), so pi
# doesn't try (and on a fresh VM, often fail with a GitHub API 403) to
# download it on first use.
RUN ARCH=$(uname -m) && \
    case ${ARCH} in \
        x86_64) FD_ARCH="x86_64-unknown-linux-gnu" ;; \
        aarch64|arm64) FD_ARCH="aarch64-unknown-linux-gnu" ;; \
        *) echo "Unsupported architecture: ${ARCH}" && exit 1 ;; \
    esac && \
    FD_VERSION=$(curl -fsSLI -o /dev/null -w '%{url_effective}' https://github.com/sharkdp/fd/releases/latest | sed 's|.*/tag/||') && \
    mkdir -p /home/exedev/.pi/agent/bin && \
    TMPDIR=$(mktemp -d) && \
    curl -fsSL "https://github.com/sharkdp/fd/releases/download/${FD_VERSION}/fd-${FD_VERSION}-${FD_ARCH}.tar.gz" | \
        tar -xz -C "${TMPDIR}" && \
    mv "${TMPDIR}/fd-${FD_VERSION}-${FD_ARCH}/fd" /home/exedev/.pi/agent/bin/fd && \
    rm -rf "${TMPDIR}" && \
    chmod 0755 /home/exedev/.pi/agent/bin/fd && \
    chown -R exedev:exedev /home/exedev/.pi/agent/bin

# Custom nginx config and index page (nginx is installed but disabled by default)
COPY nginx.conf /etc/nginx/sites-available/default
COPY index.html /var/www/html/index.html
RUN chmod 644 /var/www/html/index.html

# Install xterm-ghostty terminfo for Ghostty terminal support
COPY xterm-ghostty.terminfo /tmp/xterm-ghostty.terminfo
RUN tic -x - < /tmp/xterm-ghostty.terminfo && rm /tmp/xterm-ghostty.terminfo

# Empty the machine ID baked in by package configuration, so each VM built from
# this image generates its own on first boot. A shared one defeats anything that
# assumes machine IDs are unique, such as systemd's FixedRandomDelay=. Empty
# rather than removed: systemd reads an absent /etc/machine-id as a first boot
# and presets all units, re-enabling the ones disabled above. Keep this last, so
# that no apt-get install bakes in a new ID.
RUN : > /etc/machine-id && \
	ln -sf /etc/machine-id /var/lib/dbus/machine-id

# Expose the web server ports
EXPOSE 8000 9999

LABEL "exe.dev/login-user"="exedev"
LABEL "exe.dev/install-shelley"="true"
CMD ["/usr/local/bin/init"]
