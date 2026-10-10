#!/bin/sh
set -eu
guard=$(docker compose ps -q egress-guard)
postgres=$(docker compose ps -q postgres)
test -n "$guard" && test -n "$postgres"
network=$(docker inspect --format '{{range $name,$value := .NetworkSettings.Networks}}{{println $name}}{{end}}' "$postgres" | head -n 1)
fixture="seo-auditor-egress-test-$$"
trap 'docker rm -f "$fixture" >/dev/null 2>&1 || true' EXIT
docker run -d --name "$fixture" --network "$network" --read-only --cap-drop ALL \
    --user 65534:65534 --entrypoint /bin/sh seo-auditor-egress:local \
    -c 'while true; do printf "HTTP/1.0 200 OK\r\nContent-Length: 3\r\n\r\nok\n" | nc -l -p 18080 -w 5; done' >/dev/null
ip=$(docker inspect --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$fixture")
case "$ip" in ''|*[!0-9.]*) docker logs "$fixture" >&2; echo 'HTTP fixture не отримав IPv4' >&2; exit 1;; esac
# Спочатку доводимо, що приватний fixture відповідає без firewall.
docker run --rm --network "$network" --cap-drop ALL --entrypoint /bin/sh seo-auditor-egress:local \
    -ec "wget -Y off -T 5 -q -O /dev/null http://$ip:18080/etc/hostname"
if docker run --rm --network "container:$guard" --cap-drop ALL --entrypoint /bin/sh seo-auditor-egress:local \
    -ec "wget -Y off -T 3 -q -O /dev/null http://$ip:18080/etc/hostname"; then
    echo 'Firewall пропустив приватний HTTP target' >&2
    exit 1
fi
docker run --rm --network "container:$guard" --cap-drop ALL --entrypoint pg_isready \
    seo-auditor-postgres:local -h postgres -p 5432
docker exec "$guard" ip6tables -C OUTPUT -d 2000::/3 -p tcp -j ACCEPT
docker exec "$guard" iptables -C OUTPUT -d 169.254.0.0/16 -j REJECT
echo 'Приватний HTTP заблоковано, доступ до PostgreSQL збережено.'
