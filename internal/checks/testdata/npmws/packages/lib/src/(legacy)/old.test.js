const { old } = require('./old');

test('old', () => {
  expect(old()).toBe('old');
});
