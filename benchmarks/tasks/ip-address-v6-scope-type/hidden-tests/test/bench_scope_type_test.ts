import { expect } from 'chai';
import { Address6 } from '../src/ipv6';

// Held-out acceptance tests for Address6#getScope, #getType and #isMulticast.

const scopeOf = (address: string) => new Address6(address).getScope();
const typeOf = (address: string) => new Address6(address).getType();

describe('bench: IPv6 scope', () => {
  it('derives unicast scopes from the address type', () => {
    expect(scopeOf('fe80::1')).to.equal('Link local');
    expect(scopeOf('fe80::abcd:1234:5678:9abc')).to.equal('Link local');
    expect(scopeOf('::1')).to.equal('Link local');
    expect(scopeOf('::')).to.equal('Unknown');
    expect(scopeOf('2001:4860:4860::8888')).to.equal('Global');
    expect(scopeOf('2002:c000:204::1')).to.equal('Global');
    expect(scopeOf('2a00:1450:4001:82a::200e')).to.equal('Global');
    expect(scopeOf('fd12:3456:789a::1')).to.equal('Global');
  });

  it('keeps using the scope field for multicast addresses', () => {
    expect(scopeOf('ff01::1')).to.equal('Interface local');
    expect(scopeOf('ff02::1')).to.equal('Link local');
    expect(scopeOf('ff12::1234')).to.equal('Link local');
    expect(scopeOf('ff05::1:3')).to.equal('Site local');
    expect(scopeOf('ff08::42')).to.equal('Organization local');
    expect(scopeOf('ff0e::101')).to.equal('Global');
    expect(scopeOf('ff03::1')).to.equal('Unknown');
  });
});

describe('bench: IPv6 type', () => {
  it('recognizes unique local addresses', () => {
    expect(typeOf('fc00::1')).to.equal('Unique local');
    expect(typeOf('fdff:ffff::42')).to.equal('Unique local');
  });

  it('recognizes 6to4, documentation and NAT64 prefixes', () => {
    expect(typeOf('2002:c000:204::1')).to.equal('6to4');
    expect(typeOf('2001:db8:85a3::8a2e:370:7334')).to.equal('Documentation');
    expect(typeOf('64:ff9b::192.0.2.33')).to.equal('NAT64 (well-known)');
    expect(typeOf('64:ff9b:1:2::1')).to.equal('NAT64 (local-use)');
  });

  it('leaves the existing classifications alone', () => {
    expect(typeOf('::1')).to.equal('Loopback');
    expect(typeOf('::')).to.equal('Unspecified');
    expect(typeOf('fe80::1')).to.equal('Link-local unicast');
    expect(typeOf('ff02::2')).to.equal('Multicast (All routers on this link)');
    expect(typeOf('ff0e::101')).to.equal('Multicast');
    expect(typeOf('2001:4860:4860::8888')).to.equal('Global unicast');
  });
});

describe('bench: IPv6 multicast detection', () => {
  it('treats well-known multicast groups as multicast', () => {
    for (const address of ['ff01::1', 'ff02::1', 'ff02::1:2', 'ff05::fb', 'ff0e::101']) {
      expect(new Address6(address).isMulticast(), address).to.equal(true);
    }
  });

  it('does not treat unicast addresses as multicast', () => {
    for (const address of ['fe80::1', '::1', '2001:db8::1', 'fd00::1']) {
      expect(new Address6(address).isMulticast(), address).to.equal(false);
    }
  });
});
