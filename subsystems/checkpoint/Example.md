./linkspan --fork-command 'for i in $(seq 1 100); do echo "$i"; sleep 1; done >> /tmp/linkspan-fork-loop.log 2>&1' --shutdown-on-fork-completion true --criu-path /home/dimuthu/code/criu/criu/criu --checkpoint-fork-after-delay 5

./linkspan --restore-path "p-1784476530131977038" --shutdown-on-fork-completion false --criu-path /home/dimuthu/code/criu/criu/criu