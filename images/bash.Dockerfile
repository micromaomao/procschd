FROM alpine
RUN apk update && apk add bash
USER nobody:nobody
ENTRYPOINT [ "bash" ]
