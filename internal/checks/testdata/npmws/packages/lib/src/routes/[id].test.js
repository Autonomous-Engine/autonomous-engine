const { routeId } = require('./[id]');

test('routeId', () => {
  expect(routeId({ id: 7 })).toBe('7');
});
