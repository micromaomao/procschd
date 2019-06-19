FROM maowtm/archlinux-texlive

COPY entrypoint /entrypoint
RUN chmod a+rx /entrypoint
ENTRYPOINT [ "/entrypoint" ]
WORKDIR /tmp/
USER nobody:nobody
