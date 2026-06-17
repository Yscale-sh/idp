# openprophet public router — stock nginx + the path-fanout conf baked in (so the
# prod image is a pinned ghcr/sha artifact, no :latest, no mounted ConfigMap).
FROM nginx:1.27-alpine
COPY nginx.conf /etc/nginx/nginx.conf
EXPOSE 80
