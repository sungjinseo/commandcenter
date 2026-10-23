const isNumber = require('is-number');

exports.greet = () => 'hello ' + (isNumber(7) ? 7 : 0);
