import 'reflect-metadata';
import * as library from '../../src';
import { ClassTransformer } from '../../src/ClassTransformer';
import { Exclude, Expose, Type } from '../../src';

// Held-out acceptance tests for the class* -> instance* naming. The API is reached through a
// loosely typed handle so this file compiles before the rename.
const api: any = library;

class Address {
  street: string;

  get label(): string {
    return `@${this.street}`;
  }
}

class User {
  id: number;

  @Expose({ name: 'display_name' })
  name: string;

  @Exclude()
  password: string;

  @Type(() => Address)
  address: Address;
}

const plainUser = { id: 7, display_name: 'Ada', password: 'secret', address: { street: 'Main' } };

describe('bench: instance* naming', () => {
  it('plainToInstance builds class instances (single object and arrays)', () => {
    const user = api.plainToInstance(User, plainUser);
    expect(user).toBeInstanceOf(User);
    expect(user.name).toBe('Ada');
    expect(user.password).toBeUndefined();
    expect(user.address).toBeInstanceOf(Address);
    expect(user.address.label).toBe('@Main');

    const users = api.plainToInstance(User, [plainUser, { ...plainUser, id: 8 }]);
    expect(users).toHaveLength(2);
    expect(users[1]).toBeInstanceOf(User);
    expect(users[1].id).toBe(8);
  });

  it('instanceToPlain produces plain objects', () => {
    const user = Object.assign(new User(), { id: 1, name: 'Bob', password: 'x', address: Object.assign(new Address(), { street: 'Side' }) });
    const plain = api.instanceToPlain(user);
    expect(plain).toEqual({ id: 1, display_name: 'Bob', address: { street: 'Side' } });
    expect(plain).not.toBeInstanceOf(User);
  });

  it('instanceToInstance deep-clones class instances', () => {
    const user = Object.assign(new User(), { id: 2, name: 'Cy', address: Object.assign(new Address(), { street: 'Hill' }) });
    const copy = api.instanceToInstance(user);
    expect(copy).toBeInstanceOf(User);
    expect(copy).not.toBe(user);
    expect(copy.address).toBeInstanceOf(Address);
    expect(copy.address).not.toBe(user.address);
    expect(copy.address.street).toBe('Hill');
  });

  it('ClassTransformer exposes the same three methods', () => {
    const transformer: any = new ClassTransformer();
    expect(transformer.plainToInstance(User, plainUser)).toBeInstanceOf(User);
    expect(transformer.instanceToPlain(api.plainToInstance(User, plainUser))).toEqual({
      id: 7,
      display_name: 'Ada',
      address: { street: 'Main' },
    });
    expect(transformer.instanceToInstance(api.plainToInstance(User, plainUser))).toBeInstanceOf(User);
  });

  it('method decorators use the new names', async () => {
    const TransformInstanceToPlain = api.TransformInstanceToPlain;
    const TransformPlainToInstance = api.TransformPlainToInstance;
    const TransformInstanceToInstance = api.TransformInstanceToInstance;

    class Service {
      @TransformPlainToInstance(User)
      load(): any {
        return plainUser;
      }

      @TransformInstanceToPlain()
      async dump(): Promise<any> {
        return api.plainToInstance(User, plainUser);
      }

      @TransformInstanceToInstance()
      clone(user: User): any {
        return user;
      }
    }

    const service = new Service();
    expect(service.load()).toBeInstanceOf(User);
    expect(await service.dump()).toEqual({ id: 7, display_name: 'Ada', address: { street: 'Main' } });
    const original = api.plainToInstance(User, plainUser);
    const cloned = service.clone(original);
    expect(cloned).toBeInstanceOf(User);
    expect(cloned).not.toBe(original);
  });

  it('keeps classToPlain and plainToClass as working aliases', () => {
    const viaOld = api.plainToClass(User, plainUser);
    const viaNew = api.plainToInstance(User, plainUser);
    expect(viaOld).toBeInstanceOf(User);
    expect(viaOld).toEqual(viaNew);
    expect(api.classToPlain(viaOld)).toEqual(api.instanceToPlain(viaNew));
  });
});
