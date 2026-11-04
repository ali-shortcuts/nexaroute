#!/usr/bin/env python3
"""Chromium regression for embedded workflow controls with isolated API doubles.

This focused browser test exercises real keyboard focus and input behavior. It
is not a substitute for the gateway-backed browser acceptance suites.
"""
from pathlib import Path
import os
from playwright.sync_api import sync_playwright

ROOT = Path(__file__).resolve().parent.parent
source = (ROOT / "internal/httpapi/web/app.js").read_text()


def extract(start, end):
    a = source.index("  " + start)
    b = source.index("\n  " + end, a)
    return source[a:b]


def main():
    with sync_playwright() as pw:
        binary = os.environ.get("NEXAROUTE_CHROMIUM_BIN") or None
        browser = pw.chromium.launch(executable_path=binary, headless=True, args=["--no-sandbox"])
        try:
            page = browser.new_page()
            errors = []
            page.on("pageerror", lambda error: errors.append(str(error)))
            page.set_content('<div id="content"><span id="wf-dirty" hidden>Unsaved</span><div id="wf-validation"></div><input id="number" type="number"><input id="text" type="text"><input id="checkbox" type="checkbox"><textarea id="object"></textarea><textarea id="array"></textarea><button id="after">Next</button></div>')
            page.add_script_tag(content="""
              const state = { page: 'workflows', wf: { editor: { selected: 'n1', def: { id:'d', nodes: [{id:'n1',config:{}}] }, validation: {valid:true}, dirty:false }, defs: [] } };
              let notices = [], calls = [], finishSave;
              const notice = s => notices.push(s), body = data => ({body:JSON.stringify(data)}), $ = s => document.querySelector(s);
              const wfEditorValidationHTML = () => 'stale', workflowsView = () => '', bindWorkflows = () => {};
              const api = (path, opt) => { calls.push({path,opt}); return new Promise(resolve => { finishSave = resolve; }); };
            """ + extract("function wfEditorSetConfig(", "function wfEditorValidationHTML(")
                + extract("async function wfEditorPublish(", "function wfEditorRunDialog(")
                + extract("async function wfEditorSave(", "function wfEditorStartEdge(")
                + extract("async function wfEditorValidate(", "function wfEditorRunHTML(") + """
              document.querySelector('#number').addEventListener('change', e => wfEditorSetConfig('count','number',e.target));
              document.querySelector('#text').addEventListener('change', e => wfEditorSetConfig('name','string',e.target));
              document.querySelector('#checkbox').addEventListener('change', e => wfEditorSetConfig('enabled','boolean',e.target));
              document.querySelector('#object').addEventListener('change', e => wfEditorSetConfig('options','object',e.target));
              document.querySelector('#array').addEventListener('change', e => wfEditorSetConfig('items','array',e.target));
            """)
            page.locator("#number").fill("42")
            page.locator("#number").press("Tab")
            assert page.evaluate("document.activeElement.id") == "text"
            assert page.evaluate("state.wf.editor.def.nodes[0].config.count") == 42
            page.locator("#text").fill("example")
            page.locator("#text").press("Tab")
            assert page.evaluate("document.activeElement.id") == "checkbox"
            page.locator("#checkbox").check()
            assert page.evaluate("state.wf.editor.def.nodes[0].config.enabled") is True
            page.locator("#object").fill('{"x":1}')
            page.locator("#object").press("Tab")
            assert page.evaluate("state.wf.editor.def.nodes[0].config.options.x") == 1
            page.locator("#array").fill('[1,2]')
            page.locator("#array").press("Tab")
            assert page.evaluate("state.wf.editor.def.nodes[0].config.items.length") == 2
            page.locator("#object").fill('[]')
            page.locator("#object").press("Tab")
            assert page.evaluate("state.wf.editor.def.nodes[0].config.options.x") == 1
            assert page.locator("#wf-dirty").is_visible()
            page.evaluate("wfEditorPublish()")
            assert page.evaluate("calls.length") == 0
            page.evaluate("void wfEditorSave()")
            page.locator("#text").fill("typing while saving")
            page.evaluate("finishSave({})")
            page.wait_for_function("!state.wf.editor.saving")
            assert page.evaluate("document.activeElement.id") == "text"
            assert page.locator("#text").input_value() == "typing while saving"
            page.locator("#text").press("Tab")
            assert page.evaluate("state.wf.editor.def.nodes[0].config.name") == "typing while saving"
            assert page.evaluate("state.wf.editor.dirty") is True
            # Save first, then let validation resolve while an input is focused
            # with keystrokes that have not fired the change event yet.
            page.evaluate("void wfEditorSave()")
            page.evaluate("finishSave({})")
            page.wait_for_function("!state.wf.editor.saving")
            page.evaluate("void wfEditorValidate()")
            page.locator("#text").fill("typing during validation")
            page.evaluate("finishSave({valid:true})")
            page.wait_for_function("state.wf.editor.validation !== null")
            assert page.evaluate("document.activeElement.id") == "text"
            assert page.locator("#text").input_value() == "typing during validation"
            page.locator("#text").press("Tab")
            assert page.evaluate("state.wf.editor.def.nodes[0].config.name") == "typing during validation"
            assert page.evaluate("state.wf.editor.dirty") is True
            assert not errors, errors
        finally:
            browser.close()
    print("WORKFLOW CONFIG BROWSER PASS")


if __name__ == "__main__":
    main()
