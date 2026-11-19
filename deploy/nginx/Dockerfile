# The ingress image for ECS Fargate. docker-compose.prod.yml bind-mounts the files in this
# directory into the stock nginx image; Fargate has no host bind mounts, so this image
# bakes in the same nginx, same config. Nothing secret is in the image: the certificate and
# key arrive as environment variables at run time (04-tls-from-env.sh).
#
# Keep the base image identical to the one in docker-compose.prod.yml;
# scripts/check-nginx-image.sh fails if they differ.
FROM nginx:1.30.5-alpine@sha256:0985e772fb9f729e6fa0980da05fca5d9c468e870eed43071545afa9d2e27d94

COPY nginx.conf /etc/nginx/nginx.conf
COPY 04-tls-from-env.sh 05-ingress.envsh /docker-entrypoint.d/
COPY templates/ /etc/nginx/templates/
COPY conf/ /etc/nginx/ingress/
COPY cloudflare-origin-pull-ca.pem /etc/nginx/origin-pull-ca.pem
COPY staff-access.conf /etc/nginx/staff-access.conf

# Two differences from Compose, both because ECS is not Docker:
#  - The stock image ships conf.d/default.conf. Compose hides it with a tmpfs over conf.d;
#    here it is removed, and conf.d is made writable for the unprivileged user because the
#    entrypoint renders the ingress template into it.
#  - nginx.conf resolves `web` and the native API at run time through Docker's embedded DNS (127.0.0.11),
#    which does not exist on Fargate. Service Connect gives both services stable endpoint names
#    that survives redeploys, so resolve it once at start instead. The build fails if the
#    lines being rewritten change, rather than silently shipping a config that cannot work.
RUN set -eu \
 && grep -q '^    resolver 127\.0\.0\.11 valid=10s ipv6=off;$' /etc/nginx/nginx.conf \
 && grep -q 'server web:3000 resolve;' /etc/nginx/nginx.conf \
 && sed -i \
      -e '/^    resolver 127\.0\.0\.11 valid=10s ipv6=off;$/d' \
      -e 's/server web:3000 resolve;/server web:3000;/' \
      /etc/nginx/nginx.conf \
 && grep -q 'server ${INGRESS_NATIVE_UPSTREAM} resolve;' /etc/nginx/templates/ingress.conf.template \
 && sed -i 's/ resolve;/;/' /etc/nginx/templates/ingress.conf.template \
 && rm -f /etc/nginx/conf.d/default.conf \
 && chmod 0755 /docker-entrypoint.d/04-tls-from-env.sh /docker-entrypoint.d/05-ingress.envsh \
 && install -d -o 101 -g 101 -m 0700 /etc/nginx/tls \
 && chown 101:101 /etc/nginx/conf.d

USER 101:101
EXPOSE 8080 8443
