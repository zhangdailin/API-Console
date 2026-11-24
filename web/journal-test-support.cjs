const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('./test-support.cjs');

const script = fs.readFileSync(path.join(__dirname, 'static/js/logs.js'), 'utf8').trimEnd();
const closureEnd = '})();';
assert.ok(script.endsWith(closureEnd), 'journal fixture must load the enclosing script');

function element(tag = 'div') {
  const node = {
    tag, tagName: tag.toUpperCase(), children: [], textContent: '', className: '', hidden: false,
    disabled: false, dataset: {}, listeners: {}, parentNode: null,
    appendChild(child) {
      child.remove();
      child.parentNode = this;
      this.children.push(child);
      return child;
    },
    replaceChildren(...children) {
      this.children.forEach(child => { child.parentNode = null; });
      this.children = [];
      children.forEach(child => this.appendChild(child));
    },
    remove() {
      if (this.parentNode) {
        const siblings = this.parentNode.children;
        siblings.splice(siblings.indexOf(this), 1);
        this.parentNode = null;
      }
    },
    addEventListener(event, listener) { this.listeners[event] = listener; },
    setAttribute(name, value) { this[name] = String(value); },
    getAttribute(name) { return this[name] ?? null; },
    removeAttribute(name) { delete this[name]; },
    querySelectorAll(selector) {
      const result = [];
      const visit = parent => parent.children.forEach(child => {
        if (child.tagName === 'TR' && (selector === 'tr' || (selector === 'tr[data-index]' && child.dataset.index !== undefined))) result.push(child);
        visit(child);
      });
      visit(this);
      return result;
    },
  };
  const names = () => new Set(node.className.split(/\s+/).filter(Boolean));
  const toggle = (name, on = !names().has(name)) => {
    const classes = names();
    if (on) classes.add(name);
    else classes.delete(name);
    node.className = [...classes].join(' ');
    return on;
  };
  node.classList = { contains: name => names().has(name), toggle, add: name => toggle(name, true), remove: name => toggle(name, false) };
  return node;
}

function loadJournal(exposed, globals = {}, createElement = element) {
  const ids = Object.fromEntries([
    'logsRows', 'logsDetail', 'logsDetailBody', 'logsDetailTitle', 'logsDetailSub', 'logsTableWrap',
    'logsPager', 'logsPageInfo', 'logsPageNo', 'logsFirst', 'logsPrev', 'logsNext',
  ].map(id => [id, element(id === 'logsRows' ? 'tbody' : 'div')]));
  const context = vm.createContext({
    console, URLSearchParams, window: {location: {search: ''}},
    fetch: async () => { throw new Error('Unexpected journal fetch'); },
    ...globals,
    document: {
      // Defer the real startup event so fixtures can control individual reads.
      readyState: 'loading', hidden: false, addEventListener() {}, body: element('body'), createElement,
      getElementById: id => ids[id] || null,
      querySelectorAll: selector => selector === '#logsRows tr' ? ids.logsRows.querySelectorAll('tr') : [],
    },
  });
  // Expose fixture functions from the closure without replacing startup or other production statements.
  const exports = '\nglobalThis.journalTest = {' + exposed.join(',') + '};\n';
  vm.runInContext(script.slice(0, -closureEnd.length) + exports + closureEnd, context);
  return {...context.journalTest, ids, context};
}

const allText = node => [node.textContent, ...node.children.map(allText)].join(' ');
module.exports = { element, allText, loadJournal };
