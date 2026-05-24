const Derived = require('./lib/derived');
/**
 * @param {Derived} d
 */
function run(d) {
    d.start();
}
run(new Derived());
