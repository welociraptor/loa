{
  description = "Task store and runner for a multi-agent daily workflow";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = forAllSystems (system:
        let pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          # The checks are the gate: this derivation runs the tests and the
          # linter, so `nix flake check` answers "is it good" on a clean machine.
          default = pkgs.buildGoModule {
            pname = "loa";
            version = "0.0.0";
            src = self;
            vendorHash = "sha256-EwpoH+Thk8bplM8mjWEbMO1dAwvcA/gn+dZVJjtunbU=";

            nativeCheckInputs = [ pkgs.git pkgs.golangci-lint ];
            checkPhase = ''
              runHook preCheck
              # golangci-lint writes a cache and $HOME is read-only in the sandbox.
              export GOLANGCI_LINT_CACHE="$TMPDIR/golangci-lint-cache"
              go test ./...
              golangci-lint run ./...
              runHook postCheck
            '';

            meta = {
              description = "Task queue and runner for a multi-agent daily workflow";
              mainProgram = "loa";
            };
          };
        });

      checks = forAllSystems (system: {
        default = self.packages.${system}.default;
      });

      devShells = forAllSystems (system:
        let pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.mkShell {
            packages = with pkgs; [ go gopls sqlite git golangci-lint gotools ];
          };
        });
    };
}