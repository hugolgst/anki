{ pkgs ? import <nixpkgs> {} }:

pkgs.buildGoModule rec {
  pname = "gitanki";
  version = "0.0.1";

  src = ./.; # Point directly to your source directory

  vendorHash = null; 

  proxyVendor = true;

  meta = with pkgs.lib; {
    description = "Logs daily reviewed Anki cards (by word) to a TOML file";
    homepage = "https://github.com/hugolgst/gitanki";
    license = licenses.mit;
    maintainers = with maintainers; [ hugolgst ];
    platforms = platforms.unix;
  };
}
