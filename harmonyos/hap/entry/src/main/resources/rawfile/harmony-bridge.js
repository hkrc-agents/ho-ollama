/* Native host adapter only. Upstream attachment validation remains authoritative. */
(() => {
  if (location.protocol !== 'http:' || location.hostname !== '127.0.0.1') return;
  window.OLLAMA_PLATFORM = 'harmonyos';
  let selecting = false;
  function selectFiles(multiple) {
    if (selecting) return Promise.reject(new Error('A file picker is already open'));
    selecting = true;
    return new Promise((resolve, reject) => {
      const input = document.createElement('input');
      input.type = 'file';
      input.multiple = multiple;
      input.style.display = 'none';
      let finished = false;
      function finish(error, files) {
        if (finished) return;
        finished = true;
        selecting = false;
        input.remove();
        if (error) reject(error); else resolve(files);
      }
      input.addEventListener('cancel', () => finish(null, []), { once: true });
      input.addEventListener('change', async () => {
        try {
          const files = Array.from(input.files || []);
          if (files.length > 10 || files.some(f => f.size > 16 * 1024 * 1024) ||
              files.reduce((sum, f) => sum + f.size, 0) > 32 * 1024 * 1024) {
            throw new Error('Choose at most 10 files, 16 MiB each and 32 MiB total');
          }
          const results = await Promise.all(files.map(file => new Promise((done, fail) => {
            const reader = new FileReader();
            reader.onerror = () => fail(new Error('Cannot read selected file: ' + file.name));
            reader.onload = () => done({filename: file.name, path: '', dataURL: reader.result});
            reader.readAsDataURL(file);
          })));
          finish(null, results);
        } catch (error) { finish(error); }
      }, { once: true });
      document.body.appendChild(input);
      try { input.click(); } catch (error) { finish(error); }
    });
  }
  window.webview = Object.assign({}, window.webview, {
    selectMultipleFiles: () => selectFiles(true),
    selectFile: async () => (await selectFiles(false))[0] || null,
  });
})();
