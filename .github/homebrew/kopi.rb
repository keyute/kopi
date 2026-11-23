# Generated from .github/homebrew/kopi.rb in keyute/kopi by its release workflow.
# Overwritten on every release; edit the template there.
class Kopi < Formula
  desc "Coding-agent CLI built on PiG"
  homepage "https://github.com/keyute/kopi"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/keyute/kopi/releases/download/v@VERSION@/kopi_@VERSION@_darwin_arm64.tar.gz"
      sha256 "@SHA256_DARWIN_ARM64@"
    else
      url "https://github.com/keyute/kopi/releases/download/v@VERSION@/kopi_@VERSION@_darwin_amd64.tar.gz"
      sha256 "@SHA256_DARWIN_AMD64@"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/keyute/kopi/releases/download/v@VERSION@/kopi_@VERSION@_linux_arm64.tar.gz"
      sha256 "@SHA256_LINUX_ARM64@"
    else
      url "https://github.com/keyute/kopi/releases/download/v@VERSION@/kopi_@VERSION@_linux_amd64.tar.gz"
      sha256 "@SHA256_LINUX_AMD64@"
    end
  end

  def install
    bin.install "kopi"
    generate_completions_from_executable(bin/"kopi", shell_parameter_format: :cobra)
  end

  test do
    assert_match "kopi v#{version}", shell_output("#{bin}/kopi version")
  end
end
