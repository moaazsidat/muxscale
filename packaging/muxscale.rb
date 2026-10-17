# Homebrew formula for muxscale.
#
# Lives in the muxscale/homebrew-tap repository; kept here so a change to the
# build or the caveats is reviewed alongside the code that motivated it.
#
#   brew install muxscale/tap/muxscale
class Muxscale < Formula
  desc "Monitor and unblock your coding agents from your phone"
  homepage "https://muxscale.com"
  license "MIT"
  head "https://github.com/muxscale/muxscale.git", branch: "main"

  # Release stanzas are rewritten by the release job, which computes the real
  # checksums. Homebrew refuses to install without them, which is the point.
  url "https://github.com/muxscale/muxscale/archive/refs/tags/cli/v0.1.0.tar.gz"
  version "0.1.0"
  sha256 "0000000000000000000000000000000000000000000000000000000000000000"

  depends_on "go" => :build

  def install
    ldflags = %W[
      -s -w
      -X main.version=#{version}
      -X main.commit=#{tap&.installed? ? Utils.git_head : "release"}
    ]
    cd "go" do
      system "go", "build", *std_go_args(ldflags: ldflags), "./cmd/muxscale"
    end
  end

  # Deliberately no service block.
  #
  # `brew services` would install and start a daemon at install time, before the
  # user has asked for it. `muxscale up` is that step, and installing should
  # never be the moment something starts watching your agents.

  def caveats
    <<~EOS
      muxscale is installed but not running. To start it, now and at login:

        muxscale up

      It sets up Claude Code's hooks as it starts, backing up
      ~/.claude/settings.json first. To use it from your phone:

        muxscale connect

      `muxscale down` removes everything muxscale set up.
    EOS
  end

  test do
    assert_match "muxscale", shell_output("#{bin}/muxscale help")
    # doctor exits non-zero when the setup is incomplete, which it always is in
    # a sandbox, so only the output is asserted.
    assert_match(/claude|daemon/, shell_output("#{bin}/muxscale doctor 2>&1", 1))
  end
end
