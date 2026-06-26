import json
from pathlib import Path

from playwright.sync_api import expect, sync_playwright


BASE_URL = "http://127.0.0.1:4174"
OUT_DIR = Path("/tmp/aiops-github-webtodo-e2e-20260626-093809/final-verify/screenshots")
LOG_PATH = Path("/tmp/aiops-github-webtodo-e2e-20260626-093809/final-verify/browser-console.json")


def attach_console(page, messages):
    page.on(
        "console",
        lambda msg: messages.append(
            {
                "type": msg.type,
                "text": msg.text,
                "location": msg.location,
            }
        ),
    )


def add_todo(page, title, due_date=None):
    page.get_by_label("New todo").fill(title)
    if due_date:
        page.get_by_role("textbox", name="Due date", exact=True).fill(due_date)
    page.get_by_role("button", name="Add todo").click()


def capture_desktop_and_import(page):
    page.set_viewport_size({"width": 1440, "height": 1100})
    page.goto(BASE_URL)
    page.wait_for_load_state("networkidle")
    page.evaluate("localStorage.clear()")
    page.reload()
    page.wait_for_load_state("networkidle")

    add_todo(page, "Capture desktop checklist", "2099-03-01")
    add_todo(page, "Capture export source")
    page.get_by_role("link", name="Active").click()
    expect(page.get_by_role("region", name="Todo list")).to_contain_text(
        "Capture desktop checklist"
    )
    page.screenshot(path=str(OUT_DIR / "final-app-desktop.png"), full_page=True)

    exported = page.get_by_label("Todo JSON export").input_value()
    parsed = json.loads(exported)
    assert [todo["title"] for todo in parsed] == [
        "Capture desktop checklist",
        "Capture export source",
    ]

    imported_todos = [
        {
            "id": "final-import-1",
            "title": "Imported evidence task",
            "completed": False,
            "createdAt": "2026-06-26T09:00:00.000Z",
            "updatedAt": "2026-06-26T09:15:00.000Z",
            "dueDate": "2099-04-01",
        },
        {
            "id": "final-import-2",
            "title": "Imported completed proof",
            "completed": True,
            "createdAt": "2026-06-26T10:00:00.000Z",
            "updatedAt": "2026-06-26T10:15:00.000Z",
        },
    ]
    page.get_by_label("Todo JSON import").fill(json.dumps(imported_todos))
    page.get_by_role("button", name="Import JSON").click()
    expect(page.get_by_role("status", name="Todo activity")).to_have_text(
        "Imported 2 todos."
    )
    page.get_by_role("link", name="All").click()
    list_region = page.get_by_role("region", name="Todo list")
    expect(list_region).to_contain_text("Imported evidence task")
    expect(list_region).to_contain_text("Imported completed proof")
    page.screenshot(
        path=str(OUT_DIR / "final-app-after-import-export.png"), full_page=True
    )


def capture_mobile(page):
    page.set_viewport_size({"width": 390, "height": 900})
    page.goto(BASE_URL)
    page.wait_for_load_state("networkidle")
    page.evaluate("localStorage.clear()")
    page.reload()
    page.wait_for_load_state("networkidle")

    add_todo(
        page,
        "Mobile capture todo title wraps cleanly without horizontal overflow",
        "2099-05-01",
    )
    expect(page.get_by_role("region", name="Todo list")).to_contain_text(
        "Mobile capture todo title wraps cleanly"
    )
    overflow = page.evaluate(
        "() => ({scrollWidth: document.documentElement.scrollWidth, viewportWidth: window.innerWidth})"
    )
    assert overflow == {"scrollWidth": 390, "viewportWidth": 390}, overflow
    page.screenshot(path=str(OUT_DIR / "final-app-mobile.png"), full_page=True)


def main():
    OUT_DIR.mkdir(parents=True, exist_ok=True)
    messages = []
    with sync_playwright() as p:
        browser = p.chromium.launch(headless=True)
        context = browser.new_context()
        page = context.new_page()
        attach_console(page, messages)
        capture_desktop_and_import(page)
        context.close()

        mobile_context = browser.new_context()
        mobile_page = mobile_context.new_page()
        attach_console(mobile_page, messages)
        capture_mobile(mobile_page)
        mobile_context.close()
        browser.close()

    LOG_PATH.write_text(json.dumps(messages, indent=2), encoding="utf-8")


if __name__ == "__main__":
    main()
