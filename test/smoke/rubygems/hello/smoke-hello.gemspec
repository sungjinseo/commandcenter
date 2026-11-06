Gem::Specification.new do |s|
  s.name = "smoke-hello"
  s.version = ENV.fetch("SMOKE_VERSION", "1.0.0")
  s.summary = "commandcenter smoke test gem"
  s.authors = ["smoke"]
  s.license = "MIT"
  s.files = ["lib/smoke_hello.rb"]
  s.required_ruby_version = ">= 3.0"
  s.add_dependency "rainbow", "~> 3.1"
end
