# Reverse proxy examples for an edge node

For operators who already run a reverse proxy. `edge-agent serve` speaks plain HTTP on `EDGE_BIND`, which is `127.0.0.1:8205` by default, and your proxy adds HTTPS. Replace `edge1.example.com` with the node's hostname.

Whichever proxy you use, it must:

- Keep the path and query string unchanged, because the signature covers them.
- Pass `Range` requests through, and not cache or buffer the video.

If you have no proxy, set `COMPOSE_PROFILES=caddy` in `.env` instead.

## Caddy

```
edge1.example.com {
	reverse_proxy 127.0.0.1:8205
}
```

## nginx

```nginx
server {
    listen 443 ssl;
    server_name edge1.example.com;
    ssl_certificate     /etc/ssl/edge/fullchain.pem;
    ssl_certificate_key /etc/ssl/edge/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8205;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_buffering off;
    }
}
```

nginx can also check the signatures itself, with no `edge-agent serve`. See `../nginx/edge.conf`.

## Traefik

```yaml
http:
  routers:
    edge:
      rule: Host(`edge1.example.com`)
      service: edge
      tls:
        certResolver: letsencrypt
  services:
    edge:
      loadBalancer:
        servers:
          - url: http://127.0.0.1:8205
```
