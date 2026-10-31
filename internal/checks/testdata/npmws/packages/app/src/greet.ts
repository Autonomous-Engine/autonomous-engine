import { add, shout } from '@ws/lib';

export function greet(name: string): string {
  return shout('hello ' + name) + ' ' + add(1, 1);
}
