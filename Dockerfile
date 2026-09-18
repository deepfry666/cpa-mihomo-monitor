FROM scratch
COPY dist/mihomo-monitor-server /mihomo-monitor-server
USER 65532:65532
EXPOSE 18319
ENTRYPOINT ["/mihomo-monitor-server"]
