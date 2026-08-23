{
  # Zinc as a flake, so a NixOS/home-manager desktop can install the tools declaratively
  # instead of building them by hand. This exists for ZDE, which is a NixOS product: without
  # it there is no zc or zcr on a ZDE machine at all, and everything ZDE wants from Zinc is
  # unreachable regardless of whether Zinc implements it.
  #
  # This is a SECOND way to build the binaries, and the repo's first golden rule is that tool
  # builds are podman-only. That rule is about how Zinc builds and tests itself: `make check`
  # and `make repro` in the digest-pinned container stay the gate, and this flake is not
  # allowed to become the thing that decides whether a change is good. What it is for is
  # consumers - a flake input a desktop can pin. Nixpkgs pins the Go toolchain by hash the
  # same way the Containerfile pins it by digest, so both paths are reproducible; they are
  # just reproducible against different pins, and only one of them gates the repo.
  description = "Zinc - a security-focused sandboxing core (rootless Podman containers and qemu VMs)";

  # nixpkgs only. Deliberately no flake-utils: a consumer pinning this flake inherits every
  # input it declares, and a desktop that already has a nixpkgs wants to override one input,
  # not three.
  #
  # Pinned to an immutable revision, NOT to the `nixos-unstable` branch. A branch ref is
  # resolved afresh on every evaluation, so the same Zinc tag would build against whatever
  # nixpkgs happened to be current - a different Go compiler on two machines, and a CI job
  # that can go red on a commit that changed nothing. That also made the claim below false:
  # nixpkgs pins packages by hash WITHIN a revision, and the revision is the thing that has
  # to be pinned for it to mean anything. Refresh deliberately, the way the Go image digest
  # is refreshed.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/0954f7ee2f6bb3dc7d4e3d0d8bcb8fd4bde4cfc5";

  outputs = { self, nixpkgs }:
    let
      # aarch64 is here because the tools are pure Go and nothing in them is x86-specific.
      # zvr is the exception in practice - it drives qemu-system-x86_64 - but that is a
      # runtime dependency, not a build one, so the binary still builds and simply has
      # nothing to run.
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = fn: nixpkgs.lib.genAttrs systems (system: fn nixpkgs.legacyPackages.${system});

      # Kept in step with RELEASES.md by hand. It reaches the binary as main.version, the same
      # symbol the Makefile stamps from `git describe`, so `zc version` answers the same way
      # whichever path built it.
      version = "0.10.1";

      tools = {
        zc = {
          modRoot = "creator";
          description = "Zinc creator - define container and VM apps";
        };
        zcr = {
          modRoot = "container/runner";
          description = "Zinc container runner - launch and supervise a container app";
        };
        zvr = {
          modRoot = "virtualization/runner";
          description = "Zinc virtualization runner - launch and supervise a VM app";
        };
        zlt = {
          modRoot = "launcher/tui";
          description = "Zinc launcher (TUI) - fuzzy picker over the defined apps";
        };
        zlg = {
          modRoot = "launcher/gui";
          description = "Zinc launcher (GUI) - the same picker as a Wayland overlay";
        };
      };

      mkTool = pkgs: name: tool: pkgs.buildGoModule {
        pname = "zinc-${name}";
        inherit version;
        src = self;
        inherit (tool) modRoot;

        # Dependencies are vendored in-tree (`make vendor`), which is what null means here:
        # take vendor/, fetch nothing. It also means this build needs no network, matching
        # the hermetic property the container build has.
        vendorHash = null;

        env = {
          # Without this the repo-root go.work puts the build in workspace mode, which
          # ignores the module's own vendor/ and then fails looking for what it did not
          # fetch. The Makefiles set GOWORK=off for the same reason.
          GOWORK = "off";
          # Match the container build: the tools are pure Go by policy, and a cgo-linked
          # binary here would differ from the one `make build` produces for no reason.
          CGO_ENABLED = "0";
        };

        # Same stamping the Makefile does, so a Nix-built binary reports its version rather
        # than "dev".
        ldflags = [ "-s" "-w" "-X main.version=v${version}" ];

        subPackages = [ "." ];

        # `go build .` names the binary after the module's last path element, so this would
        # otherwise install bin/creator, bin/tui, and - twice - bin/runner, which collide the
        # moment the tools are joined into one package. The Makefile renames on the way out of
        # the build container for the same reason; this is that rename.
        postInstall = ''
          mv "$out/bin/${baseNameOf tool.modRoot}" "$out/bin/${name}"
        '';

        # The test suites are the repo's own gate and run in the pinned container via
        # `make check`, where podman and a writable HOME are available. Running them again
        # inside the Nix sandbox would test the sandbox, not the code, so this build only
        # builds. A consumer wanting the gate runs `make check`.
        doCheck = false;

        meta = with pkgs.lib; {
          inherit (tool) description;
          homepage = "https://github.com/crispuscrew/zinc";
          license = licenses.asl20;
          platforms = platforms.linux;
          mainProgram = name;
        };
      };
    in
    {
      packages = forAllSystems (pkgs:
        let built = nixpkgs.lib.mapAttrs (name: tool: mkTool pkgs name tool) tools;
        in built // {
          # Everything, for `nix profile install` and for the home-manager module's default.
          default = pkgs.symlinkJoin {
            name = "zinc-${version}";
            paths = builtins.attrValues built;
            meta.description = "Zinc - all five tools (zc, zcr, zvr, zlt, zlg)";
          };
        });

      # `nix build` builds each tool; this is what CI runs.
      checks = forAllSystems (pkgs: nixpkgs.lib.mapAttrs (name: tool: mkTool pkgs name tool) tools);

      # home-manager module. ZDE installs Zinc in its layer 1, so this is the shape it
      # arrives in: enable it, get the tools on PATH.
      #
      # `tools` defaults to the container-side three rather than all five, because that is
      # what a desktop needs to define and run apps. zvr is opt-in since a VM runner without
      # qemu on the machine is a binary that cannot work, and zlg is opt-in because a desktop
      # shipping its own launcher does not want a second one on PATH.
      homeModules.zinc = { config, lib, pkgs, ... }:
        let cfg = config.programs.zinc;
        in {
          options.programs.zinc = {
            enable = lib.mkEnableOption "the Zinc sandboxing tools";

            tools = lib.mkOption {
              type = lib.types.listOf (lib.types.enum (builtins.attrNames tools));
              default = [ "zc" "zcr" "zlt" ];
              example = [ "zc" "zcr" "zvr" "zlt" "zlg" ];
              description = ''
                Which Zinc tools to put on PATH. zc authors app files, zcr runs container
                apps, zvr runs VM apps, zlt and zlg are the terminal and Wayland launchers.

                zc shells out to whichever runner owns an app, so installing zc without zcr
                gives you authoring and no way to run what you authored.
              '';
            };

            packages = lib.mkOption {
              type = lib.types.attrsOf lib.types.package;
              default = self.packages.${pkgs.stdenv.hostPlatform.system};
              defaultText = lib.literalExpression "zinc.packages.\${system}";
              description = "The package set the selected tools are taken from. Override to build Zinc from a different source.";
            };
          };

          config = lib.mkIf cfg.enable {
            home.packages = map (name: cfg.packages.${name}) cfg.tools;

            # Deliberately NOT done here: installing podman or qemu. Both are system-level on
            # NixOS (virtualisation.podman, and the kvm group for qemu), a home-manager module
            # cannot enable them, and pulling copies into the user profile would produce a
            # second podman that does not share the system's storage or its rootless setup.
            # The tools report a missing runtime clearly when it is absent.
            warnings = lib.optional (!(builtins.elem "zc" cfg.tools) && cfg.tools != [ ])
              "programs.zinc: no zc, so there is nothing to author the apps the installed runners run.";
          };
        };

      # The old name, kept because home-manager modules were `homeManagerModules` before they
      # were `homeModules` and a consumer may pin either.
      homeManagerModules = self.homeModules;
    };
}
