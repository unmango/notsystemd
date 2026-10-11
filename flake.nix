{
  description = "Pod-native stand-in for systemd, for software that expects to run under it";

  nixConfig = {
    extra-substituters = [
      "https://mangopkgs.cachix.org"
      "https://unstoppablemango.cachix.org"
    ];
    extra-trusted-public-keys = [
      "mangopkgs.cachix.org-1:uJ5FgSbOg1uiXLcL0gBh1lO+y3KVuthy6UeOFYR1fLk="
      "unstoppablemango.cachix.org-1:m7uEI6X1Ov8DyFWJQX4WsRFRWFuzRW5c/Xms8ZaP74U="
    ];
  };

  inputs = {
    # nixpkgs follows mangopkgs, so that Go and friends are the store paths
    # its cachix cache already holds.
    mangopkgs.url = "github:unmango/pkgs";
    nixpkgs.follows = "mangopkgs/nixpkgs";
    systems.url = "github:UnstoppableMango/nix-systems";

    flake-parts = {
      url = "github:hercules-ci/flake-parts";
      inputs.nixpkgs-lib.follows = "nixpkgs";
    };

    treefmt-nix = {
      url = "github:numtide/treefmt-nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };

    gomod2nix = {
      url = "github:nix-community/gomod2nix";
      inputs.nixpkgs.follows = "nixpkgs";
      inputs.flake-utils.inputs.systems.follows = "systems";
    };
  };

  outputs =
    inputs@{ flake-parts, ... }:
    flake-parts.lib.mkFlake { inherit inputs; } {
      systems = import inputs.systems;
      imports = with inputs; [
        systems.flakeModule
        treefmt-nix.flakeModule
      ];

      perSystem =
        { pkgs, system, ... }:
        let
          notsystemd = pkgs.callPackage ./nix/package.nix { };
        in
        {
          _module.args.pkgs = import inputs.nixpkgs {
            inherit system;
            overlays = with inputs; [ gomod2nix.overlays.default ];
          };

          packages = {
            inherit notsystemd;
            default = notsystemd;

            # sudo, systemctl, systemd-run and timedatectl, each a link to
            # notsystemd, which answers as the command it is invoked as. A
            # separate package so that installing notsystemd shadows nothing.
            shims = pkgs.runCommand "notsystemd-shims" { } ''
              mkdir -p $out/bin
              for name in sudo systemctl systemd-run timedatectl; do
                ln -s ${pkgs.lib.getExe notsystemd} $out/bin/$name
              done
            '';
          };

          devShells.default = pkgs.mkShellNoCC {
            packages = with pkgs; [
              direnv
              ginkgo
              gnumake
              go
              gomod2nix
              gopls
              nixfmt
            ];
          };

          treefmt.programs = {
            actionlint.enable = true;
            gofmt.enable = true;
            mdformat.enable = true;
            nixfmt.enable = true;
          };
        };
    };
}
