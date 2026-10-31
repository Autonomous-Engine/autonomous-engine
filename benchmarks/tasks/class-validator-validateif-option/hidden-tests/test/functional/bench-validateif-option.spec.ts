import { IsString, Length, Max, Min, registerDecorator, ValidationOptions } from '../../src';
import { Validator } from '../../src/validation/Validator';

// Held-out acceptance tests for the per-decorator `validateIf` validation option.

const validator = new Validator();

// Builds decorator options; typed loosely so the file also compiles before the option exists.
const options = (validateIf: (object: any, value: any) => boolean, extra: Record<string, unknown> = {}): any => ({
  ...extra,
  validateIf,
});

function constraintNames(errors: { constraints?: Record<string, string> }[]): string[] {
  return errors.flatMap(e => Object.keys(e.constraints ?? {})).sort();
}

describe('bench: validateIf validation option', () => {
  class Ranged {
    @Min(10, options((o: Ranged) => o.mode !== 'max-only'))
    @Max(2, options((o: Ranged) => o.mode !== 'min-only'))
    amount: number;

    mode: string;
  }

  const make = (amount: number, mode: string) => Object.assign(new Ranged(), { amount, mode });

  it('skips only the decorators whose condition is false', async () => {
    expect(constraintNames(await validator.validate(make(5, 'min-only')))).toEqual(['min']);
    expect(constraintNames(await validator.validate(make(5, 'max-only')))).toEqual(['max']);
    expect(constraintNames(await validator.validate(make(5, '')))).toEqual(['max', 'min']);
  });

  it('applies to validateSync as well', () => {
    expect(constraintNames(validator.validateSync(make(5, 'min-only')))).toEqual(['min']);
    expect(constraintNames(validator.validateSync(make(5, '')))).toEqual(['max', 'min']);
  });

  it('passes the object being validated and the property value to the condition', async () => {
    const calls: [unknown, unknown][] = [];
    class Probe {
      @Length(3, 5, options((object, value) => {
        calls.push([object, value]);
        return true;
      }))
      name: string;
    }
    const probe = Object.assign(new Probe(), { name: 'ab' });
    const errors = await validator.validate(probe);
    expect(constraintNames(errors)).toEqual(['isLength']);
    expect(calls.length).toBeGreaterThan(0);
    expect(calls[0][0]).toBe(probe);
    expect(calls[0][1]).toBe('ab');
  });

  it('keeps the other decorators on the same property running', async () => {
    class Mixed {
      @IsString()
      @Length(10, 20, options(() => false))
      label: unknown;
    }
    expect(constraintNames(await validator.validate(Object.assign(new Mixed(), { label: 'short' })))).toEqual([]);
    expect(constraintNames(await validator.validate(Object.assign(new Mixed(), { label: 42 })))).toEqual(['isString']);
  });

  it('works together with each: true', async () => {
    class Tags {
      @IsString(options((o: Tags) => o.strict, { each: true }))
      tags: unknown[];

      strict: boolean;
    }
    expect(await validator.validate(Object.assign(new Tags(), { tags: ['a', 1], strict: false }))).toHaveLength(0);
    expect(constraintNames(await validator.validate(Object.assign(new Tags(), { tags: ['a', 1], strict: true })))).toEqual([
      'isString',
    ]);
  });

  it('is honoured by custom decorators created with registerDecorator', async () => {
    function IsEven(validationOptions?: ValidationOptions) {
      return function (object: object, propertyName: string) {
        registerDecorator({
          name: 'isEven',
          target: object.constructor,
          propertyName,
          options: validationOptions,
          validator: { validate: (value: any) => typeof value === 'number' && value % 2 === 0 },
        });
      };
    }
    class Counter {
      @IsEven(options((o: Counter) => o.enabled))
      count: number;

      enabled: boolean;
    }
    expect(await validator.validate(Object.assign(new Counter(), { count: 3, enabled: false }))).toHaveLength(0);
    expect(constraintNames(await validator.validate(Object.assign(new Counter(), { count: 3, enabled: true })))).toEqual([
      'isEven',
    ]);
  });
});
