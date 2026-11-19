{
  description = "Dicta voice dictation for Wayland";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
    in {
      apps = nixpkgs.lib.genAttrs systems (system:
        let
          package = self.packages.${system}.dicta;
          mkApp = name: {
            type = "app";
            program = "${package}/bin/${name}";
          };
        in rec {
          default = dicta;
          dicta = mkApp "dicta";
          dictad = mkApp "dictad";
        });

      packages = nixpkgs.lib.genAttrs systems (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          ydotool = "${pkgs.ydotool}/bin/ydotool";
          wlCopy = "${pkgs.wl-clipboard}/bin/wl-copy";
        in rec {
          default = dicta;
          dicta = pkgs.buildGoModule {
            pname = "dicta";
            version = "0.1.0-dev";
            src = self;
            vendorHash = "sha256-x/FdKVcxgIES7WeXmv6RUIHdvNHCenRr/9SGC7GE8OM=";

            nativeBuildInputs = with pkgs; [ pkg-config makeWrapper ];
            buildInputs = with pkgs; [ wayland libxkbcommon libglvnd vulkan-headers ];

            env.CGO_ENABLED = "0";
            subPackages = [ "cmd/dicta" "cmd/dictad" ];
            ldflags = [ "-s" "-w" ];

            preBuild = ''
              substituteInPlace cmd/dictad/main.go \
                --replace-fail /usr/bin/ydotool ${ydotool} \
                --replace-fail /usr/bin/wl-copy ${wlCopy} \
                --replace-fail '"preview-binary", ""' "\"preview-binary\", \"$out/bin/dicta-preview\""

              substituteInPlace internal/dispatch/typer.go \
                --replace-fail '"/opt"}' '"/opt", "${ydotool}", "${wlCopy}"}'

              substituteInPlace cmd/dictad/preview.go \
                --replace-fail '"/opt"}' "\"/opt\", \"$out/bin/dicta-preview\"}"
            '';

            postBuild = ''
              CGO_ENABLED=1 go build -tags nox11 -trimpath -ldflags="-s -w" \
                -o "$GOPATH/bin/dicta-preview" ./cmd/dicta-preview
            '';

            postInstall = ''
              wrapProgram "$out/bin/dictad" \
                --prefix PATH : ${pkgs.lib.makeBinPath [ pkgs.pipewire pkgs.wireplumber pkgs.libnotify ]}
            '';

            meta.mainProgram = "dicta";
          };
        });
    };
}
