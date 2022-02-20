FROM golang:alpine AS build
WORKDIR /usr/src/app
COPY ./server .
RUN go get && go build

FROM alpine
COPY --from=build /usr/src/app/procschd /procschd
ENTRYPOINT [ "/procschd" ]
CMD [ "--listen", "tcp:127.0.0.1:3000" ]
