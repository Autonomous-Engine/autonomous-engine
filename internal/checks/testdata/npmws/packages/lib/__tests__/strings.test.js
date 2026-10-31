const { shout } = require('../src/strings');

test('shout', () => {
  expect(shout('hi')).toBe('HI!');
});
