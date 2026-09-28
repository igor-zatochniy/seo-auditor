const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const source = fs.readFileSync(path.join(__dirname, "../static/dialogs.js"), "utf8");

function fixture() {
  const listeners = {};
  const dialog = {
    open: true,
    getBoundingClientRect: () => ({ left: 100, right: 500, top: 50, bottom: 400 }),
    addEventListener: (name, callback) => { listeners[name] = callback; },
    close() { this.open = false; listeners.close(); }
  };
  vm.runInNewContext(source, { document: { querySelectorAll: () => [dialog] } });
  const fire = (name, x, y, extra = {}) => listeners[name]({ target: dialog, clientX: x, clientY: y, button: 0, isPrimary: true, ...extra });
  return { dialog, fire };
}

test("all four sides dismiss the dialog", () => {
  for (const [x, y] of [[20, 150], [550, 150], [200, 20], [200, 450]]) {
    const { dialog, fire } = fixture();
    fire("pointerdown", x, y); fire("click", x, y);
    assert.equal(dialog.open, false);
  }
});
test("content, padding and text selection do not dismiss", () => {
  for (const [start, end] of [[[110, 60], [110, 60]], [[200, 100], [20, 100]], [[20, 100], [200, 100]]]) {
    const { dialog, fire } = fixture();
    fire("pointerdown", ...start); fire("click", ...end);
    assert.equal(dialog.open, true);
  }
  const { dialog, fire } = fixture();
  fire("pointerdown", 20, 100, { target: {} }); fire("click", 20, 100);
  assert.equal(dialog.open, true);
});
test("canceled gestures and secondary buttons do not dismiss", () => {
  const { dialog, fire } = fixture();
  fire("pointerdown", 20, 100); fire("pointercancel"); fire("click", 20, 100);
  assert.equal(dialog.open, true);
  fire("pointerdown", 20, 100, { button: 2 }); fire("click", 20, 100, { button: 2 });
  assert.equal(dialog.open, true);
  fire("pointerdown", 20, 100, { isPrimary: false }); fire("click", 20, 100);
  assert.equal(dialog.open, true);
});
