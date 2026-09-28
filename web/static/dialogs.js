"use strict";
(() => {
  for (const dialog of document.querySelectorAll("dialog")) {
    let pressedBackdrop = false;
    const isBackdrop = event => {
      const rect = dialog.getBoundingClientRect();
      return event.target === dialog &&
        (event.clientX < rect.left || event.clientX > rect.right ||
         event.clientY < rect.top || event.clientY > rect.bottom);
    };
    dialog.addEventListener("pointerdown", event => {
      pressedBackdrop = event.button === 0 && event.isPrimary !== false && isBackdrop(event);
    });
    dialog.addEventListener("click", event => {
      const dismiss = pressedBackdrop && event.button === 0 && isBackdrop(event);
      pressedBackdrop = false;
      if (dismiss && dialog.open) dialog.close();
    });
    dialog.addEventListener("pointercancel", () => { pressedBackdrop = false; });
    dialog.addEventListener("close", () => { pressedBackdrop = false; });
  }
})();
