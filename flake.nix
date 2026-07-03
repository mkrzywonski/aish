{
  description = "aish — AI-shareable terminal";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.11";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAll = f: nixpkgs.lib.genAttrs systems f;
      rev = self.shortRev or self.dirtyShortRev or "dev";
    in
    {
      overlays.default = final: prev: {
        aish = final.callPackage ./package.nix { inherit rev; };
      };

      packages = forAll (system:
        let
          pkgs = import nixpkgs { inherit system; overlays = [ self.overlays.default ]; };
        in
        {
          default = pkgs.aish;
          aish = pkgs.aish;
        });

      devShells = forAll (system:
        let pkgs = import nixpkgs { inherit system; };
        in { default = pkgs.mkShell { packages = [ pkgs.go ]; }; });
    };
}
