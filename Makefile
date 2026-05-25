.PHONY: build test install uninstall clean

build:
	go build -o shrimp-basket main.go

test:
	go test -v ./...

install: build
	@printf "==> Creating local binary directory and cache directory...\n"
	mkdir -p $(HOME)/.local/bin
	mkdir -p -m 0700 $(HOME)/.cache/shrimp-basket
	cp shrimp-basket $(HOME)/.local/bin/shrimp-basket
	chmod +x $(HOME)/.local/bin/shrimp-basket

	@printf "==> Installing systemd user units...\n"
	mkdir -p $(HOME)/.config/systemd/user
	cp systemd/shrimp-basket.socket $(HOME)/.config/systemd/user/
	cp systemd/shrimp-basket.service $(HOME)/.config/systemd/user/
	cp systemd/shrimp-basket-update.service $(HOME)/.config/systemd/user/
	cp systemd/shrimp-basket-update.timer $(HOME)/.config/systemd/user/

	@printf "==> Activating systemd socket & timer...\n"
	systemctl --user daemon-reload
	systemctl --user enable --now shrimp-basket.socket
	systemctl --user enable --now shrimp-basket-update.timer

	@printf "==> Backing up and configuring NPM global registry in ~/.npmrc...\n"
	@if [ -f $(HOME)/.npmrc ]; then \
		if [ ! -f $(HOME)/.npmrc.shrimp-basket.bak ]; then \
			cp $(HOME)/.npmrc $(HOME)/.npmrc.shrimp-basket.bak; \
		fi; \
		if grep -q "^registry=" $(HOME)/.npmrc; then \
			sed -i 's|^registry=.*|registry=http://127.0.0.1:12345/|' $(HOME)/.npmrc; \
		else \
			printf "registry=http://127.0.0.1:12345/\n" >> $(HOME)/.npmrc; \
		fi; \
	else \
		printf "registry=http://127.0.0.1:12345/\n" > $(HOME)/.npmrc; \
	fi

	@printf "==> Backing up and configuring UV index-url in ~/.config/uv/uv.toml...\n"
	@mkdir -p $(HOME)/.config/uv
	@if [ -f $(HOME)/.config/uv/uv.toml ]; then \
		if [ ! -f $(HOME)/.config/uv/uv.toml.shrimp-basket.bak ]; then \
			cp $(HOME)/.config/uv/uv.toml $(HOME)/.config/uv/uv.toml.shrimp-basket.bak; \
		fi; \
		if grep -q "^index-url" $(HOME)/.config/uv/uv.toml; then \
			sed -i 's|^index-url.*|index-url = "http://127.0.0.1:12345/simple"|' $(HOME)/.config/uv/uv.toml; \
		else \
			printf 'index-url = "http://127.0.0.1:12345/simple"\n' >> $(HOME)/.config/uv/uv.toml; \
		fi; \
	else \
		printf 'index-url = "http://127.0.0.1:12345/simple"\n' > $(HOME)/.config/uv/uv.toml; \
	fi

	@printf "==> Backing up and configuring PIP index-url in ~/.config/pip/pip.conf...\n"
	@mkdir -p $(HOME)/.config/pip
	@if [ -f $(HOME)/.config/pip/pip.conf ]; then \
		if [ ! -f $(HOME)/.config/pip/pip.conf.shrimp-basket.bak ]; then \
			cp $(HOME)/.config/pip/pip.conf $(HOME)/.config/pip/pip.conf.shrimp-basket.bak; \
		fi; \
		if grep -q "^index-url" $(HOME)/.config/pip/pip.conf; then \
			sed -i 's|^index-url.*|index-url = http://127.0.0.1:12345/simple|' $(HOME)/.config/pip/pip.conf; \
		elif grep -q "^\[global\]" $(HOME)/.config/pip/pip.conf; then \
			sed -i 's|\[global\]|\[global\]\nindex-url = http://127.0.0.1:12345/simple|' $(HOME)/.config/pip/pip.conf; \
		else \
			printf "[global]\nindex-url = http://127.0.0.1:12345/simple\n" >> $(HOME)/.config/pip/pip.conf; \
		fi; \
	else \
		printf "[global]\nindex-url = http://127.0.0.1:12345/simple\n" > $(HOME)/.config/pip/pip.conf; \
	fi

	@printf "Installation successful! shrimp-basket is running via socket activation on port 12345.\n"

uninstall:
	@printf "==> Stopping and disabling systemd units...\n"
	systemctl --user stop shrimp-basket.service 2>/dev/null || true
	systemctl --user disable --now shrimp-basket.socket 2>/dev/null || true
	systemctl --user disable --now shrimp-basket-update.timer 2>/dev/null || true
	
	@printf "==> Removing systemd unit files...\n"
	rm -f $(HOME)/.config/systemd/user/shrimp-basket.socket
	rm -f $(HOME)/.config/systemd/user/shrimp-basket.service
	rm -f $(HOME)/.config/systemd/user/shrimp-basket-update.service
	rm -f $(HOME)/.config/systemd/user/shrimp-basket-update.timer
	systemctl --user daemon-reload

	@printf "==> Removing executable...\n"
	rm -f $(HOME)/.local/bin/shrimp-basket

	@printf "==> Restoring NPM registry configuration...\n"
	@if [ -f $(HOME)/.npmrc.shrimp-basket.bak ]; then \
		mv -f $(HOME)/.npmrc.shrimp-basket.bak $(HOME)/.npmrc; \
	else \
		rm -f $(HOME)/.npmrc; \
	fi

	@printf "==> Restoring UV index-url configuration...\n"
	@if [ -f $(HOME)/.config/uv/uv.toml.shrimp-basket.bak ]; then \
		mv -f $(HOME)/.config/uv/uv.toml.shrimp-basket.bak $(HOME)/.config/uv/uv.toml; \
	else \
		rm -f $(HOME)/.config/uv/uv.toml; \
	fi

	@printf "==> Restoring PIP index-url configuration...\n"
	@if [ -f $(HOME)/.config/pip/pip.conf.shrimp-basket.bak ]; then \
		mv -f $(HOME)/.config/pip/pip.conf.shrimp-basket.bak $(HOME)/.config/pip/pip.conf; \
	else \
		rm -f $(HOME)/.config/pip/pip.conf; \
	fi

	@printf "Uninstall complete. Original configs and defaults restored.\n"

clean:
	rm -f shrimp-basket
