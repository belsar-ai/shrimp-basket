# shrimp-basket
shrimp-basket is a zero-dependency Go proxy that sits between your package manager and PyPI/npm, stripping any release published in the last 7 days. Runs on-demand via systemd socket activation; idles at zero RAM.
