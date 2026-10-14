# frozen_string_literal: true

# ruby test/routes_test.rb — exercises ./routes against an in-memory fake of
# the Cloudflare API, a scratch routes.yml and a scratch Caddyfile.
require "minitest/autorun"
require "stringio"
require "tmpdir"

DIR = Dir.mktmpdir("routes-test")
ENV["ROUTES_FILE"] = File.join(DIR, "routes.yml")
ENV["ROUTES_CADDYFILE"] = File.join(DIR, "Caddyfile")
load File.expand_path("../routes", __dir__)

Routes::CLI.prepend(Module.new { def running_tunnel_id = nil })

class FakeApi
  TID = "tid-1"
  attr_reader :config, :records, :calls

  def initialize(ingress:, records: [])
    @config = { "ingress" => ingress, "warp-routing" => { "enabled" => false } }
    @records = records
    @calls = []
  end

  def accounts = [{ "id" => "acct", "name" => "Me" }]
  def tunnels(_) = [{ "id" => TID, "name" => "macmini", "config_src" => "cloudflare" }]
  def tunnel_config(_, _) = Marshal.load(Marshal.dump(@config))
  def zones(_) = [{ "id" => "z1", "name" => "grillermo.com" }, { "id" => "z2", "name" => "chiq.me" }]
  def dns_records(zone_id) = @records.select { |r| r["zone"] == zone_id }

  def put_tunnel_config(_, _, config)
    @calls << [:put, config]
    @config = config
  end

  def create_dns_record(zone_id, record)
    @calls << [:create, record["name"]]
    @records << record.merge("zone" => zone_id, "id" => "r#{@records.length}")
  end

  def delete_dns_record(_, id)
    @calls << [:delete, id]
    @records.reject! { |r| r["id"] == id }
  end
end

class RoutesTest < Minitest::Test
  TARGET = "#{FakeApi::TID}.cfargotunnel.com"

  def setup
    FileUtils.rm_f(Dir[File.join(DIR, "*")])
    File.write(ENV["ROUTES_CADDYFILE"], ":3020 {\n\treverse_proxy localhost:3021\n}\n")
    @api = FakeApi.new(
      ingress: [
        { "hostname" => "cal.grillermo.com", "service" => "http://localhost:3020", "originRequest" => {} },
        { "hostname" => "rulinky.grillermo.com", "service" => "http://localhost:3500", "originRequest" => { "noTLSVerify" => true } },
        { "service" => "http_status:404" },
      ],
      records: [
        { "zone" => "z1", "id" => "a", "type" => "CNAME", "name" => "cal.grillermo.com", "content" => TARGET },
        { "zone" => "z1", "id" => "b", "type" => "CNAME", "name" => "rulinky.grillermo.com", "content" => TARGET },
        { "zone" => "z1", "id" => "c", "type" => "A", "name" => "taken.grillermo.com", "content" => "1.2.3.4" },
      ],
    )
    @reloads = 0
    @reload_ok = true
    run!("pull")
  end

  def run!(*argv)
    out = StringIO.new
    status = Routes::CLI.new(argv, api: @api, out: out, reload: -> { @reloads += 1; @reload_ok }).run
    assert_equal 0, status
    out.string
  end

  def registry = Routes::Registry.load(ENV["ROUTES_FILE"])
  def caddyfile = File.read(ENV["ROUTES_CADDYFILE"])
  def hosts = @api.config["ingress"].map { |r| r["hostname"] }

  def test_pull_writes_registry_and_sync_is_then_a_no_op
    yml = File.read(ENV["ROUTES_FILE"])
    assert_includes yml, "  cal.grillermo.com: http://localhost:3020\n"
    assert_includes yml, "    originRequest:\n      noTLSVerify: true\n"
    assert_includes run!("sync"), "Everything already matches"
    assert_empty @api.calls
  end

  def test_create_adds_route_and_dns_keeping_other_config
    out = run!("create", "new.chiq.me", "8081")
    assert_includes out, "+ route  new.chiq.me -> http://localhost:8081"
    assert_equal ["cal.grillermo.com", "rulinky.grillermo.com", "new.chiq.me", nil], hosts
    assert_equal({ "enabled" => false }, @api.config["warp-routing"])
    assert_equal({ "noTLSVerify" => true }, @api.config["ingress"][1]["originRequest"])
    assert_includes @api.calls, [:create, "new.chiq.me"]
    assert_equal "z2", @api.records.last["zone"]
    assert_equal 0, @reloads
  end

  def test_dry_run_changes_nothing
    run!("create", "new.chiq.me", "8081", "--dry-run")
    assert_empty @api.calls
    refute registry.routes.key?("new.chiq.me")
  end

  def test_create_with_caddy_writes_block_and_reloads
    run!("create", "sub.grillermo.com", "http://localhost:8081", "--caddy", "8080")
    assert_equal({ "service" => "http://localhost:8080", "caddy" => "http://localhost:8081" }, registry.routes["sub.grillermo.com"])
    assert_includes caddyfile, ":8080 {\n\treverse_proxy http://localhost:8081\n}\n# routes:end sub.grillermo.com\n"
    assert_equal 1, @reloads

    run!("modify", "sub.grillermo.com", "8082")
    assert_includes caddyfile, "reverse_proxy http://localhost:8082"
    assert_equal "http://localhost:8080", registry.routes["sub.grillermo.com"]["service"]
    assert_equal 2, @reloads

    run!("delete", "sub.grillermo.com")
    assert_equal ":3020 {\n\treverse_proxy localhost:3021\n}\n", caddyfile
    refute_includes hosts, "sub.grillermo.com"
    refute(@api.records.any? { |r| r["name"] == "sub.grillermo.com" })
  end

  def test_caddy_port_clash_is_rejected_before_anything_changes
    error = assert_raises(Routes::Error) { run!("create", "sub.grillermo.com", "8081", "--caddy", "3020") }
    assert_match(/:3020 is already used/, error.message)
    refute registry.routes.key?("sub.grillermo.com")
    assert_empty @api.calls
  end

  def test_failed_reload_restores_caddyfile_and_skips_cloudflare
    before = caddyfile
    @reload_ok = false
    assert_raises(Routes::Error) { run!("create", "sub.grillermo.com", "8081", "--caddy", "8080") }
    assert_equal before, caddyfile
    assert_empty @api.calls
  end

  def test_modify_changes_only_the_route
    out = run!("modify", "rulinky.grillermo.com", "http://localhost:3600")
    assert_includes out, "~ route  rulinky.grillermo.com"
    assert_equal "http://localhost:3600", @api.config["ingress"][1]["service"]
    assert_equal [:put], @api.calls.map(&:first)
  end

  def test_remove_deletes_route_and_cname
    run!("remove", "rulinky.grillermo.com")
    assert_equal ["cal.grillermo.com", nil], hosts
    assert_includes @api.calls, [:delete, "b"]
  end

  def test_existing_non_tunnel_record_is_not_clobbered
    error = assert_raises(Routes::Error) { run!("create", "taken.grillermo.com", "9000") }
    assert_match(/already has DNS A 1.2.3.4/, error.message)
    assert_empty @api.calls
  end

  def test_unknown_zone_syncs_route_but_skips_dns
    out = run!("create", "x.antesis.com.mx", "9000")
    assert_includes out, "! dns    x.antesis.com.mx: skipped"
    assert_includes hosts, "x.antesis.com.mx"
    assert_equal [:put], @api.calls.map(&:first)
  end

  def test_orphan_cname_to_this_tunnel_is_deleted
    @api.records << { "zone" => "z2", "id" => "o", "type" => "CNAME", "name" => "old.chiq.me", "content" => TARGET }
    assert_includes run!("sync"), "- dns    old.chiq.me"
    assert_equal [[:delete, "o"]], @api.calls
  end

  def test_hand_edited_yaml_is_synced
    File.write(ENV["ROUTES_FILE"], File.read(ENV["ROUTES_FILE"]).sub("3020", "3030"))
    run!("sync")
    assert_equal "http://localhost:3030", @api.config["ingress"][0]["service"]
  end

  def test_pull_keeps_caddy_and_note
    run!("create", "sub.grillermo.com", "8081", "--caddy", "8080")
    File.write(ENV["ROUTES_FILE"], File.read(ENV["ROUTES_FILE"]).sub("\n    caddy: http://localhost:8081\n", "\n    caddy: http://localhost:8081\n    note: my thing\n"))
    run!("pull")
    assert_equal({ "service" => "http://localhost:8080", "caddy" => "http://localhost:8081", "note" => "my thing" }, registry.routes["sub.grillermo.com"])
  end
end
