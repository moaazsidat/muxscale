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
  # user has agreed to hooks, shims, or a tailnet. `muxscale up` does that
  # interactively and shows each change first, and installing should never be
  # the moment something starts watching your agents.

  def caveats
    <<~EOS
      muxscale is installed but not running. To set it up:

        muxscale up

      That will offer, one at a time and showing each change first:
        - a login service, so the daemon starts with your machine
        - Claude Code hooks, for exact blocked-state detection and approvals
        - PATH shims, so sessions you start can accept replies

      Nothing is installed or started without you agreeing to it.

      To see what is already running without changing anything:

        muxscale daemon &
        muxscale ls
    EOS
  end

  test do
    assert_match "muxscale", shell_output("#{bin}/muxscale help")
    # doctor exits non-zero when the setup is incomplete, which it always is in
    # a sandbox, so only the output is asserted.
    assert_match(/claude|daemon/, shell_output("#{bin}/muxscale doctor 2>&1", 1))
  end
end
