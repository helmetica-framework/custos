FROM gcr.io/distroless/static:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5

ENTRYPOINT ["/usr/bin/custos"]
COPY custos /usr/bin/
