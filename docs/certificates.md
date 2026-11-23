# DNS and certificates

Every artifact is a subdomain of the base domain, so Kazari needs a wildcard DNS record and a wildcard certificate. This page covers the DNS records, how Kazari obtains the certificate, the DNS providers it can use and what to do with any other, the configurations of a proxy in front, and the problems that come up. It assumes you have chosen a domain, as in [before you start](install.md#before-you-start).

The examples use `art.example.com` as the base domain, held in the zone `example.com`.

## How the certificate is obtained

A certificate for `*.art.example.com` can only be issued by the DNS-01 challenge: the certificate authority asks for a TXT record at `_acme-challenge.art.example.com` and checks that it appears. The HTTP challenge cannot issue wildcards. So Kazari needs an API token for the DNS provider that holds the zone, writes the record through it, and removes it afterwards.

Kazari orders two certificates through the same DNS-01 flow: one for `art.example.com` and one for `*.art.example.com`, which covers every host under it. A certificate per slug would need port 80 open to the world, run into Let's Encrypt's weekly limit per domain, and list every slug in the public certificate-transparency logs, including those of `members` pages.

`tls.mode` decides where the certificate comes from. Most instances use `acme`; use `files` when another tool manages the certificate, and `self-signed` for tests:

| `tls.mode`    | Certificates                                 | Use for                              |
| ------------- | -------------------------------------------- | ------------------------------------ |
| `acme`        | Obtained from Let's Encrypt by DNS-01        | Production (default)                 |
| `files`       | PEM files you provide, reloaded when they change | Another DNS provider, or your own CA |
| `self-signed` | Generated at start                           | Development and tests                |

In proxy mode none of this applies: the proxy holds the certificate, and Kazari serves plain HTTP ([running behind a reverse proxy](install.md#running-behind-a-reverse-proxy)).

## DNS records

Two records point the base domain and everything under it at the server:

```text
art.example.com.      A     203.0.113.10
*.art.example.com.    A     203.0.113.10
```

Three things about wildcard records catch people out:

- The wildcard does not cover the apex. `art.example.com` needs its own record.
- A record for a specific name wins over the wildcard. A `www.art.example.com` record of its own would send that one name elsewhere.
- Add `AAAA` records only if the server really has working IPv6. A broken IPv6 path makes certificate issuance fail even while IPv4 works, because the authority may prefer IPv6.

Check the apex, a fixed host and a made-up one, which only the wildcard answers:

```sh
dig +short art.example.com
dig +short api.art.example.com
dig +short anything.art.example.com
```

Each prints `203.0.113.10`. `kazari check` does the same three lookups and compares them with the server's own addresses ([checking the setup](install.md#checking-the-setup)).

## Providers

`tls.dns_provider` names the provider and `tls.dns.api_token` holds its credential. The token needs permission to add and delete records in the zone that holds the base domain, and nothing more:

| Provider     | `tls.dns_provider` | `tls.dns.api_token`                                                              |
| ------------ | ------------------ | -------------------------------------------------------------------------------- |
| Cloudflare   | `cloudflare`       | An API token with `Zone.DNS:Edit` on the zone                                    |
| AWS Route 53 | `route53`          | Empty, for the AWS default credential chain, or `ACCESS_KEY_ID:SECRET_ACCESS_KEY` |
| Hetzner DNS  | `hetzner`          | A Hetzner DNS API token                                                          |
| DigitalOcean | `digitalocean`     | A personal access token with write scope                                         |
| Gandi        | `gandi`            | A personal access token allowed to manage domain technical configuration         |
| OVH          | `ovh`              | `APPLICATION_KEY:APPLICATION_SECRET:CONSUMER_KEY`                                |
| Namecheap    | `namecheap`        | The API key                                                                      |
| DuckDNS      | `duckdns`          | The account token                                                                |

Two providers take more than the token. OVH reads `tls.dns.endpoint`, `ovh-eu` by default, `ovh-ca` or `ovh-us`. Namecheap needs `tls.dns.user`, the API user, and allow-lists the calling IP in its console; `tls.dns.client_ip` names that address, or Kazari discovers the server's public address when it is empty.

Keep the token out of `kazari.yaml`, which refers to it as `${KAZARI_TLS_DNS_API_TOKEN}`, and put it in the dotenv file. Then prove the token works, with a TXT record written and removed through the provider's API:

```sh
sudo kazari check
```

The `dns token` line reports `wrote and removed a TXT record in zone example.com.`. When the provider refuses, it says what to do first, then quotes the provider's error: a token in the wrong format or revoked, a token without permission on the zone, or a zone the token's account does not hold.

## Using another DNS provider

For a provider Kazari does not know, issue the wildcard certificate with [lego](https://go-acme.github.io/lego/), which supports well over a hundred providers, and serve it from files. Kazari watches the files and reloads them within a minute of a change, so renewals need no restart.

Issue the certificate with lego, using its environment variables for your provider:

```sh
sudo lego --email ops@example.com --dns <provider> \
  --domains art.example.com --domains '*.art.example.com' \
  --path /etc/kazari/lego run
```

Point Kazari at the files it wrote, in place of the `tls` section with a provider:

```yaml
tls:
  mode: files
  cert_file: /etc/kazari/lego/certificates/art.example.com.crt
  key_file: /etc/kazari/lego/certificates/art.example.com.key
```

Then renew on a schedule, with the same command and `renew` in place of `run`, for example from a daily timer or cron job. certbot works the same way with its DNS plugins.

`kazari check` opens both files as the server does, and its `cert files` line names the hosts the certificate covers and its expiry date, or fails when it does not cover the base domain and its wildcard.

### Letting the service read the files

The systemd unit runs Kazari as a dynamic user, which exists only while the service runs and owns no files. Give the files to a group instead, and add the service to that group. Create the group and hand it the files:

```sh
sudo groupadd --system kazari-tls
sudo chgrp -R kazari-tls /etc/kazari/lego
sudo chmod 0750 /etc/kazari/lego /etc/kazari/lego/certificates
sudo chmod 0640 /etc/kazari/lego/certificates/*
```

Then uncomment `SupplementaryGroups=kazari-tls` in the unit and restart it. lego writes renewed files with its own mode, so repeat the `chgrp` and `chmod` in the renewal job.

systemd's `LoadCredential=` is an alternative: it copies each file into `/run/credentials/kazari.service/` at start, readable by the service alone. Point `cert_file` and `key_file` at the copies there. A renewal then needs `systemctl restart kazari`, because the copies change only at start.

## Proxy examples

In proxy mode the proxy obtains the wildcard certificate and forwards every host to Kazari on `127.0.0.1:8080`, with the `Host` header unchanged. Whichever proxy you use, raise its request body limit to at least a third more than `limits.max_version_bytes` (about 70 MiB for the default 50 MiB), since inline files arrive in base64, or large publishes are refused by the proxy before Kazari sees them. The proxy needs no compression of its own: Kazari [compresses text files](configuration.md#compression) itself.

### Caddy

Caddy obtains the certificate with its own DNS module for the provider. The `Caddyfile` in `deploy/` forwards the apex and every subdomain:

```caddyfile
*.art.example.com, art.example.com {
	tls ops@example.com {
		dns cloudflare {env.CLOUDFLARE_API_TOKEN}
	}

	reverse_proxy 127.0.0.1:8080 {
		header_up X-Forwarded-Host {host}
		header_up X-Real-IP {remote_host}
	}
}
```

The `dns cloudflare` directive needs a Caddy build with the `caddy-dns/cloudflare` module. The [download page](https://caddyserver.com/download) builds one with the modules you pick, and a running Caddy adds one with `caddy add-package github.com/caddy-dns/cloudflare`. For another provider, use its module instead.

### nginx

nginx serves a certificate issued by another tool, such as lego as in [using another DNS provider](#using-another-dns-provider). One server block matches the apex and every subdomain:

```nginx
server {
    listen 443 ssl;
    listen [::]:443 ssl;
    http2 on;
    server_name art.example.com .art.example.com;

    ssl_certificate     /etc/kazari/lego/certificates/art.example.com.crt;
    ssl_certificate_key /etc/kazari/lego/certificates/art.example.com.key;
    client_max_body_size 70m;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-Proto https;
    }
}

server {
    listen 80;
    listen [::]:80;
    server_name art.example.com .art.example.com;
    return 301 https://$host$request_uri;
}
```

nginx sets `X-Real-IP` to the address it saw, the header Kazari reads with the default `listen.client_ip_header`.

Reload nginx after each renewal, since it reads the files at start.

### Traefik

Traefik issues wildcard certificates with a certificate resolver that uses the `dnsChallenge` for your provider; its HTTP challenge cannot. Give the router the apex and the wildcard as the certificate's domains and the catch-all host rule:

```yaml
http:
  routers:
    kazari:
      rule: "HostRegexp(`^.+\\.art\\.example\\.com$`) || Host(`art.example.com`)"
      service: kazari
      tls:
        certResolver: letsencrypt
        domains:
          - main: art.example.com
            sans: ["*.art.example.com"]
  services:
    kazari:
      loadBalancer:
        servers:
          - url: http://127.0.0.1:8080
```

Traefik's own documentation covers the resolver's `dnsChallenge` settings for each provider.

### Cloudflare Tunnel

A tunnel reaches a server with no open ports: a `*` CNAME record pointing at the tunnel and one published hostname `*.art.example.com` route every subdomain to Kazari in proxy mode. The tunnel passes the client's address in `X-Forwarded-For`, so set `listen.client_ip_header: X-Forwarded-For`. Cloudflare's free Universal SSL certificates cover one label under the zone, so they serve `*.example.com` but not `*.art.example.com`. A base domain with two labels under the zone needs Cloudflare's Advanced Certificate Manager or a certificate you upload; a base domain directly under the zone, such as `example.com` itself or `art.example.com` as the zone, does not.

## Troubleshooting

### The certificate is not issued

With `tls.mode: acme`, Kazari obtains the certificate in the background after start, and TLS handshakes fail until it has one. The log says `obtaining certificate`, then `certificate obtained` or `certificate not obtained` with the error and a `hint` chosen by the kind of error. Under systemd, filter for them:

```sh
journalctl -u kazari | grep -i certificate
```

`kazari admin ready` reports the same: its `certificate` line warns when there is still no certificate five minutes after start, and fails with the last error once an order has failed.

The usual causes:

- **DNS provider errors** (`401`, `403`, "zone not found"): the token lacks permission on the zone. A Cloudflare token needs `Zone.DNS:Edit` for the zone that contains `base_domain`. `kazari check` reproduces this without waiting for an order.
- **Challenge not seen**: DNS-01 writes a `_acme-challenge.art.example.com` TXT record and waits for it to propagate. Check it with `dig TXT _acme-challenge.art.example.com @1.1.1.1` while it runs.
- **CA unreachable**: the host cannot make outbound HTTPS connections to `acme-v02.api.letsencrypt.org`, through a firewall or a proxy.
- **CAA records**: they must allow `letsencrypt.org`.
- **Rate limits**: repeated failures against production Let's Encrypt hit its limits. Debug with `tls.ca: letsencrypt-staging`, then switch back and delete `certmagic/` from the data directory to drop the staging certificate.
- **`files` mode**: the files are checked once a minute, and a reload failure keeps the old certificate and logs `certs: reload failed, keeping previous certificate`.

### A host does not resolve

Every host must resolve to the server. Check the apex, a fixed host and any other name, as in [DNS records](#dns-records). A wildcard record does not cover the apex, and an explicit record for a subdomain, such as `www`, overrides the wildcard for that name.

An artifact host that resolves but shows the instance 404 page means DNS and TLS work, and the slug is unknown, unpublished or deleted.

### `ERR_SSL_VERSION_OR_CIPHER_MISMATCH` behind Cloudflare

The browser reached Cloudflare, which has no certificate for the name. Universal SSL covers one label under the zone, so `hello.art.example.com` is out of its reach; see [Cloudflare Tunnel](#cloudflare-tunnel).

## Next steps

- [Installing Kazari](install.md): the paths to a running instance.
- [Configuration](configuration.md#tls): every `tls` key and its default.
- [Operations](ops.md): backups, upgrades and logs.
