# Local test target: an SSH server and an XFCE desktop on a virtual screen,
# standing in for a Linux PC with someone logged in at its monitor.
FROM debian:trixie-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      xvfb xauth dbus-x11 x11-utils x11-xserver-utils \
      xfce4-session xfwm4 xfce4-panel xfdesktop4 xfce4-settings xfce4-terminal thunar mousepad \
      adwaita-icon-theme fonts-dejavu-core \
      ffmpeg ca-certificates \
 && rm -rf /var/lib/apt/lists/*
RUN apt-get update \
 && apt-get install -y --no-install-recommends openssh-server \
 && rm -rf /var/lib/apt/lists/*
# xclip lets tests put text on the X clipboard and read it back.
RUN apt-get update \
 && apt-get install -y --no-install-recommends xclip \
 && rm -rf /var/lib/apt/lists/*

RUN useradd --create-home --shell /bin/bash desk
RUN echo 'desk:desk' | chpasswd && mkdir -p /run/sshd

# Docker hides the container's IP from the browser, so pin the host's WebRTC
# traffic to one published port and advertise it as localhost. sshd passes
# /etc/environment to SSH sessions through pam_env.
RUN printf 'WEBDESK_ICE_PORT=50000\nWEBDESK_ICE_HOST_IP=127.0.0.1\n' >> /etc/environment

COPY test/start-target.sh test/start-desktop.sh /usr/local/bin/
CMD ["start-target.sh"]
