// A route-style file name: jest reads filters as regular expressions, so
// the selected filter must escape the brackets.
function routeId(params) {
  return String(params.id);
}

module.exports = { routeId };
