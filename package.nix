{ lib
, buildGoModule
, rev ? "dev"
}:

buildGoModule {
  pname = "aish";
  version = "0.2.2-${rev}";

  src = lib.cleanSource ./.;

  # Changes only when go.mod dependencies change; nix prints the new value
  # on mismatch.
  vendorHash = "sha256-2APx8BpXR2+PvXpyNDlfEVO1AwVrob6QmEaWV4cS4/Q=";

  subPackages = [ "cmd/aish" ];

  ldflags = [ "-s" "-w" "-X main.version=0.2.2-${rev}" ];

  meta = with lib; {
    description = "AI-shareable terminal: human and MCP client drive one shared shell session";
    homepage = "https://github.com/mkrzywonski/aish";
    license = licenses.gpl3Only;
    platforms = platforms.linux;
    mainProgram = "aish";
  };
}
