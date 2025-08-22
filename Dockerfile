FROM ubuntu:24.04

# Switch from dash to bash by default.
SHELL ["/bin/bash", "-euxo", "pipefail", "-c"]

# Remove minimization restrictions and install packages with documentation
# We aim for a usable non-minimal system.
RUN rm -f /etc/dpkg/dpkg.cfg.d/excludes /etc/dpkg/dpkg.cfg.d/01_nodoc && \
	apt-get update && \
	# Pre-configure debconf to avoid interactive prompts
	echo 'debconf debconf/frontend select Noninteractive' | debconf-set-selections && \
	# Pre-configure pbuilder to avoid mirror prompt
	echo 'pbuilder pbuilder/mirrorsite string http://archive.ubuntu.com/ubuntu' | debconf-set-selections && \
	# Run unminimize with single 'y' response to restore documentation
	echo 'y' | DEBIAN_FRONTEND=noninteractive unminimize && \
	# Install man-db and reinstall all base packages to get their man pages back
	DEBIAN_FRONTEND=noninteractive apt-get install -y man-db && \
	DEBIAN_FRONTEND=noninteractive apt-get install -y --reinstall $(dpkg-query -f '${binary:Package} ' -W) && \
	mandb -c && \
	DEBIAN_FRONTEND=noninteractive apt-get install -y \
		ca-certificates wget \
		git jq sqlite3 curl vim lsof iproute2 less \
		make python3-pip python-is-python3 tree net-tools file build-essential \
		pipx psmisc bsdmainutils sudo socat \
		openssh-server openssh-client \
		iputils-ping socat netcat-openbsd \
		unzip util-linux rsync \
		ubuntu-server ubuntu-dev-tools ubuntu-standard \
		man-db manpages manpages-dev && \
	apt-get clean && \
	rm -rf /var/lib/apt/lists/*

# Modify existing ubuntu user (UID 1000) to become exedev user
RUN usermod -l exedev ubuntu && \
	groupmod -n exedev ubuntu && \
	mv /home/ubuntu /home/exedev && \
	usermod -d /home/exedev exedev && \
	usermod -aG sudo exedev && \
	echo 'exedev ALL=(ALL) NOPASSWD:ALL' >> /etc/sudoers

ENV GO_VERSION=1.25.0
ENV GOROOT=/usr/local/go
ENV GOPATH=/go
ENV PATH=$GOROOT/bin:$GOPATH/bin:$PATH

RUN ARCH=$(uname -m) && \
	case $ARCH in \
		x86_64) GOARCH=amd64 ;; \
		aarch64) GOARCH=arm64 ;; \
		*) echo "Unsupported architecture: $ARCH" && exit 1 ;; \
	esac && \
	wget -O go.tar.gz "https://golang.org/dl/go${GO_VERSION}.linux-${GOARCH}.tar.gz" && \
	tar -C /usr/local -xzf go.tar.gz && \
	rm go.tar.gz

# Create GOPATH directory and set ownership
RUN mkdir -p "$GOPATH/src" "$GOPATH/bin" && chmod -R 755 "$GOPATH" && chown -R exedev:exedev "$GOPATH"

# Install Go tools and clean up aggressively
RUN go install golang.org/x/tools/cmd/goimports@latest; \
	go install golang.org/x/tools/gopls@latest; \
	go install mvdan.cc/gofumpt@latest; \
	go clean -cache -testcache -modcache && \
	rm -rf /usr/local/go/pkg/tool/*/test2json \
		/usr/local/go/pkg/tool/*/vet \
		/usr/local/go/pkg/tool/*/doc \
		/usr/local/go/test \
		/usr/local/go/api \
		/usr/local/go/doc

ENV GOTOOLCHAIN=auto
ENV EXEUNTU=1

# Add claude script to PATH (in /usr/bin to avoid conflict with npm's /usr/local/bin/claude)
COPY claude /usr/bin/claude
RUN chmod +x /usr/bin/claude

# Set default user to exedev
USER exedev
