#!/bin/sh
set -eu

postgres_host=${EGRESS_POSTGRES_HOST:-postgres}
browser_host=${EGRESS_BROWSER_HOST:-}
for host in "$postgres_host" "$browser_host"; do
    case "$host" in *[!A-Za-z0-9_.-]*) echo 'Некоректне ім’я сервісу firewall' >&2; exit 1;; esac
done

resolve_ipv4() {
    getent hosts "$1" | awk '$1 ~ /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/ { print $1 }' | sort -u
}

install_rules() {
    pg_ips=$(resolve_ipv4 "$postgres_host")
    test -n "$pg_ips"
    browser_ips=''
    if test -n "$browser_host"; then
        browser_ips=$(resolve_ipv4 "$browser_host")
        test -n "$browser_ips"
    fi
    {
        printf '*filter\n:INPUT ACCEPT [0:0]\n:FORWARD DROP [0:0]\n:OUTPUT DROP [0:0]\n'
        printf '%s\n' '-A OUTPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT'
        # Docker DNS перенаправляє порт 53 на власний локальний порт.
        printf '%s\n' '-A OUTPUT -d 127.0.0.11/32 -p udp -m conntrack --ctorigdstport 53 -j ACCEPT' '-A OUTPUT -d 127.0.0.11/32 -p tcp -m conntrack --ctorigdstport 53 -j ACCEPT'
        printf '%s\n' '-A OUTPUT -d 127.0.0.1/32 -p tcp --dport 8080 -j ACCEPT'
        for ip in $pg_ips; do printf '%s\n' "-A OUTPUT -d $ip/32 -p tcp --dport 5432 -j ACCEPT"; done
        for ip in $browser_ips; do printf '%s\n' "-A OUTPUT -d $ip/32 -p tcp --dport 9222 -j ACCEPT"; done
        for cidr in 0.0.0.0/8 10.0.0.0/8 100.64.0.0/10 127.0.0.0/8 169.254.0.0/16 172.16.0.0/12 192.0.0.0/24 192.0.2.0/24 192.88.99.0/24 192.168.0.0/16 198.18.0.0/15 198.51.100.0/24 203.0.113.0/24 224.0.0.0/4 240.0.0.0/4; do
            printf '%s\n' "-A OUTPUT -d $cidr -j REJECT"
        done
        printf '%s\n' '-A OUTPUT -p tcp -j ACCEPT' 'COMMIT'
    } > /tmp/ipv4.rules
    {
        printf '*filter\n:INPUT ACCEPT [0:0]\n:FORWARD DROP [0:0]\n:OUTPUT DROP [0:0]\n'
        printf '%s\n' '-A OUTPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT'
        for cidr in 2001::/23 2001:db8::/32 2002::/16; do printf '%s\n' "-A OUTPUT -d $cidr -j REJECT"; done
        # NDP потрібен для IPv6 routing; це не дозвіл TCP/UDP до локальних сервісів.
        for kind in 133 135 136; do
            printf '%s\n' "-A OUTPUT -p ipv6-icmp --icmpv6-type $kind -m hl --hl-eq 255 -j ACCEPT"
        done
        # Дозволено лише global-unicast IPv6; NAT64, ULA, link-local та mapped IPv4 закрито.
        printf '%s\n' '-A OUTPUT -d 2000::/3 -p tcp -j ACCEPT' 'COMMIT'
    } > /tmp/ipv6.rules
    iptables-restore --wait 5 < /tmp/ipv4.rules
    ip6tables-restore --wait 5 < /tmp/ipv6.rules
}

# Parser запускається лише після healthcheck: обидві таблиці вже встановлено.
install_rules
touch /tmp/firewall-ready
trap 'rm -f /tmp/firewall-ready; exit 0' TERM INT
while sleep 15 & wait $!; do
    if ! install_rules; then
        rm -f /tmp/firewall-ready
        echo 'Не вдалося оновити firewall; попередні обмеження залишено' >&2
        exit 1
    fi
done
