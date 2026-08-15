{ lib
, buildGoModule
, rev ? "dev"
}:

buildGoModule {
  pname = "aish";
  version = rev;

  src = lib.cleanSource ./.;

  # Changes only when go.mod dependencies change; nix prints the new value
  # on mismatch.
  vendorHash = "sha256-bvPJsvMb2PCbruScI5QQ4YxV+4HZzx4+Qlr17YZgSjY=";

  subPackages = [ "cmd/aish" ];

  ldflags = [ "-s" "-w" "-X main.version=${rev}" ];

  meta = with lib; {
    description = "AI-shareable terminal: human and MCP client drive one shared shell session";
    homepage = "https://github.com/mkrzywonski/aish";
    license = licenses.gpl3Only;
    platforms = platforms.linux;
    mainProgram = "aish";
  };
}
