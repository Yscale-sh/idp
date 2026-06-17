# anyrent public router — stock nginx + the host-fanout conf baked in (pinned
# ghcr/sha artifact for prod; no :latest, no mounted ConfigMap).
FROM nginx:1.27-alpine
COPY nginx.conf /etc/nginx/nginx.conf
EXPOSE 80
