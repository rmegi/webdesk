#!/bin/sh
# Test target entrypoint: starts sshd, then the desktop session as user "desk".
set -eu

# Let the dev server's SSH key log in when the password is left empty.
if [ -f /etc/webdesk/ssh/id_ed25519.pub ]; then
  install -d -m 700 -o desk -g desk /home/desk/.ssh
  install -m 600 -o desk -g desk /etc/webdesk/ssh/id_ed25519.pub /home/desk/.ssh/authorized_keys
fi

install -d -m 1777 /tmp/.X11-unix
/usr/sbin/sshd
exec su desk -c start-desktop.sh
