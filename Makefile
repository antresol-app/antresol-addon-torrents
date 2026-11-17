# An Antresol addon's release (docs/addons/contract.md in the core).
#   make build   dist/: one archive per platform (bin/addon), the web part
#                (<id>-web.tar.gz) and antresol-addon.json with their sha256
#   make local   dist-local/<id>/: the same for this machine only, for a dev
#                server started with VAULT_ADDONS_DEV=<this dir>/dist-local
#                (install «local:<id>» in «Расширения»)
#   make test    go vet and the Go tests; with a core checkout
#                (ANTRESOL_CORE) also tools/corecopy.sh --check
#   make corecopy  internal/corecopy/ anew from the core checkout
#                (tools/corecopy.list)
# The program builds on addonkit (go.mod's require). The web part builds on
# the SDK package @antresol/addon-sdk: ANTRESOL_ADDON_SDK=<its packed .tgz>,
# else one packed from the core checkout's web/addon-sdk (its web/ must have
# run `npm ci`). ANTRESOL_CORE defaults to the core repository this tree
# sits in, if any.

ID := $(shell sed -n 's/^ *"id": *"\([^"]*\)".*/\1/p' antresol-addon.json | head -n 1)
# the core repository this tree sits in, if any: git's top level when it
# holds addonkit/go.mod (as tools/corecopy.sh and TestCorecopyInSync find it);
# found once
CORE_DETECTED := $(shell top=$$(git rev-parse --show-toplevel 2>/dev/null) && [ -f "$$top/addonkit/go.mod" ] && printf '%s' "$$top")
ANTRESOL_CORE ?= $(CORE_DETECTED)
ANTRESOL_ADDON_SDK ?=
PLATFORMS := linux/amd64 linux/arm64 darwin/arm64
HOST := $(shell GOENV=off go env GOOS)/$(shell GOENV=off go env GOARCH)

# a static, reproducible binary: no GOFLAGS or go env settings of a
# surrounding build, no cgo, no paths or VCS stamp of this checkout
export GOFLAGS :=
export GOENV := off
export CGO_ENABLED := 0
GOBUILD := go build -trimpath -buildvcs=false -ldflags='-s -w'
# plain, reproducible archives: ustar (no pax headers), entries sorted,
# owner 0, fixed modes and times, gzip without a name or time; on macOS no
# extended attributes and no AppleDouble ._ files (the core refuses them)
export COPYFILE_DISABLE := 1
BSDTAR := $(findstring bsdtar,$(shell tar --version 2>/dev/null))
TAR := tar --format=ustar $(if $(BSDTAR),--no-mac-metadata --no-xattrs --uid 0 --gid 0 --uname root --gname root -n,--owner=root:0 --group=root:0 --no-recursion)

.PHONY: build local test corecopy web clean

build: web
	rm -rf dist build && mkdir -p dist
	$(foreach p,$(PLATFORMS),$(call pack,$(p),dist))
	$(call targz,web/dist/web,.,644,dist/$(ID)-web.tar.gz)
	go run ./tools/manifest -src antresol-addon.json -dir dist $(if $(TAG),-tag $(TAG))

local: web
	rm -rf dist-local/$(ID) build && mkdir -p dist-local/$(ID)
	$(call pack,$(HOST),dist-local/$(ID))
	$(call targz,web/dist/web,.,644,dist-local/$(ID)/$(ID)-web.tar.gz)
	go run ./tools/manifest -src antresol-addon.json -dir dist-local/$(ID)

test:
	go vet ./...
	ANTRESOL_CORE=$(ANTRESOL_CORE) go test ./...
	$(if $(ANTRESOL_CORE),sh tools/corecopy.sh --check $(ANTRESOL_CORE),@echo "no core checkout (ANTRESOL_CORE): tools/corecopy.sh --check skipped")

# the core helpers this addon builds on, copied (tools/corecopy.sh)
corecopy:
	sh tools/corecopy.sh $(ANTRESOL_CORE)

# targz DIR,ROOT,MODE,OUT: what ROOT (inside DIR) holds into OUT, its files
# at MODE, its folders at 755, all at one fixed time
define targz
find $(1) -type d -exec chmod 755 {} +
find $(1) -type f -exec chmod $(3) {} +
find $(1) -exec touch -h -t 198001010000 {} +
(cd $(1) && find $(2) | LC_ALL=C sort | $(TAR) -cf - -T -) | gzip -n -9 >$(4)
endef

# pack OS/ARCH,DIR: build/<os>-<arch>/bin/addon into DIR/<id>-<os>-<arch>.tar.gz
define pack
GOOS=$(word 1,$(subst /, ,$(1))) GOARCH=$(word 2,$(subst /, ,$(1))) $(GOBUILD) -o build/$(subst /,-,$(1))/bin/addon .
$(call targz,build/$(subst /,-,$(1)),bin,755,$(2)/$(ID)-$(subst /,-,$(1)).tar.gz)

endef

# the SDK goes into web/node_modules as a plain copy of its package
web: web/node_modules
	@set -e; sdk="$(ANTRESOL_ADDON_SDK)"; \
	if [ -z "$$sdk" ]; then \
		[ -n "$(ANTRESOL_CORE)" ] && [ -d "$(ANTRESOL_CORE)/web/addon-sdk" ] || \
			{ echo "no SDK: set ANTRESOL_ADDON_SDK=<the packed @antresol/addon-sdk> or ANTRESOL_CORE=<a core checkout>" >&2; exit 1; }; \
		npm --prefix "$(ANTRESOL_CORE)/web" run --silent sdk:types; \
		rm -rf build/sdk && mkdir -p build/sdk; \
		(cd "$(ANTRESOL_CORE)/web/addon-sdk" && npm pack --silent --ignore-scripts --pack-destination "$(CURDIR)/build/sdk") >/dev/null; \
		sdk=$$(ls build/sdk/*.tgz); \
	fi; \
	rm -rf web/node_modules/@antresol/addon-sdk && mkdir -p web/node_modules/@antresol/addon-sdk; \
	tar -xzf "$$sdk" -C web/node_modules/@antresol/addon-sdk --strip-components 1
	npm --prefix web run build

web/node_modules: web/package.json web/package-lock.json
	npm --prefix web ci --ignore-scripts
	touch web/node_modules

clean:
	rm -rf build dist dist-local web/dist
