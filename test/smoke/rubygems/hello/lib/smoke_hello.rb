require "rainbow"

module SmokeHello
  VERSION = Gem.loaded_specs["smoke-hello"].version.to_s

  def self.greet = "hello #{Rainbow(7)} from #{VERSION}"
end
