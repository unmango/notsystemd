{
  buildGoApplication,
  lib,
  stdenv,
}:
buildGoApplication {
  pname = "notsystemd";
  version = lib.trim (builtins.readFile ../VERSION);

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      (lib.fileset.fileFilter (f: f.hasExt "go") ../.)
    ];
  };

  modules = ./gomod2nix.toml;
  subPackages = [ "cmd/notsystemd" ];

  # A static binary, so an image can copy it in without a libc. Without cgo,
  # user lookups read /etc/passwd and /etc/group rather than going through
  # NSS, which is what a container has anyway.
  CGO_ENABLED = 0;
  ldflags = [
    "-s"
    "-w"
  ];

  # The supervisor specs run /bin/sh, which the Darwin sandbox has but which
  # notsystemd, a Linux-only tool, never needs there.
  doCheck = stdenv.hostPlatform.isLinux;
  checkPhase = ''
    runHook preCheck
    go test ./...
    runHook postCheck
  '';

  meta = {
    description = "Pod-native stand-in for systemd, for software that expects to run under it";
    homepage = "https://github.com/unmango/notsystemd";
    license = lib.licenses.mit;
    mainProgram = "notsystemd";
    platforms = lib.platforms.linux;
  };
}
