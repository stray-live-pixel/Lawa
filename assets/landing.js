(() => {
  "use strict";
  const slides = [...document.querySelectorAll("[data-slide]")];
  const arrows = [...document.querySelectorAll("[data-direction]")];
  const currentLabel = document.querySelector(".slide-progress__current");
  const totalLabel = document.querySelector(".slide-progress__total");
  const progressBar = document.querySelector(".slide-progress b");
  const status = document.querySelector("[data-slide-status]");
  const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  let currentSlide = 0;
  /** Обновляет общую навигацию, не вмешиваясь в нативную прокрутку страницы. */
  function selectSlide(index) {
    currentSlide = Math.max(0, Math.min(index, slides.length - 1));
    currentLabel.textContent = String(currentSlide + 1).padStart(2, "0");
    progressBar.style.height = `${((currentSlide + 1) / slides.length) * 100}%`;
    status.textContent = `Слайд ${currentSlide + 1} из ${slides.length}`;
    arrows[0].disabled = currentSlide === 0;
    arrows[1].disabled = currentSlide === slides.length - 1;
  }

  arrows.forEach((button) => {
    button.addEventListener("click", () => {
      const next = currentSlide + Number(button.dataset.direction);
      // Выравнивание слайда запускают только крупные стрелки. Обычный скролл не
      // перехватывается и не доводится до секции через CSS scroll-snap.
      slides[next]?.scrollIntoView({ behavior: reducedMotion ? "auto" : "smooth" });
    });
  });

  // Слайд считается активным по наибольшей видимой площади, поэтому длинный третий
  // экран не перескакивает раньше времени и обычный скролл остаётся предсказуемым.
  const visibleSlides = new Map();
  const slideObserver = new IntersectionObserver(
    (entries) => {
      entries.forEach((entry) => visibleSlides.set(entry.target, entry.intersectionRatio));
      const active = [...visibleSlides.entries()].sort((a, b) => b[1] - a[1])[0];
      if (active?.[1] > 0) selectSlide(Number(active[0].dataset.slide));
    },
    { threshold: [0, 0.2, 0.4, 0.6, 0.8] },
  );
  slides.forEach((slide) => slideObserver.observe(slide));
  totalLabel.textContent = String(slides.length).padStart(2, "0");
  selectSlide(0);
  const copyButton = document.querySelector("[data-copy]");
  const copySource = document.querySelector("[data-copy-source]");
  const defaultCopyLabel = copyButton?.getAttribute("aria-label") ?? "";
  const defaultCopyTitle = copyButton?.getAttribute("title") ?? "";
  let copyFeedbackTimer = 0;

  /** Показывает результат копирования, не заменяя компактную иконку текстовой кнопкой. */
  function showCopyFeedback(state, label, title) {
    window.clearTimeout(copyFeedbackTimer);
    copyButton.classList.remove("is-copied", "is-error");
    copyButton.classList.add(state);
    copyButton.setAttribute("aria-label", label);
    copyButton.setAttribute("title", title);
    copyFeedbackTimer = window.setTimeout(() => {
      copyButton.classList.remove("is-copied", "is-error");
      copyButton.setAttribute("aria-label", defaultCopyLabel);
      copyButton.setAttribute("title", defaultCopyTitle);
    }, 1800);
  }

  copyButton?.addEventListener("click", async () => {
    const prompt = copySource?.textContent.trim();
    if (!prompt) return;
    try {
      await navigator.clipboard.writeText(prompt);
      showCopyFeedback("is-copied", "Промпт скопирован", "Промпт скопирован");
    } catch {
      showCopyFeedback("is-error", "Не удалось скопировать промпт", "Выделите текст вручную");
    }
  });

  const canvas = document.querySelector(".lava-canvas");
  if (!canvas) return;
  const context = canvas.getContext("2d", { alpha: true });
  if (!context) return;
  const waves = [
    {
      base: 0.14,
      thickness: 0.12,
      amplitude: 0.035,
      frequency: 1.35,
      speed: 0.00026,
      phase: 0.4,
      colors: ["rgba(65, 11, 3, 0.3)", "rgba(132, 31, 7, 0.54)", "rgba(180, 66, 10, 0.58)"],
    },
    {
      base: 0.4,
      thickness: 0.16,
      amplitude: 0.048,
      frequency: 1.12,
      speed: -0.0002,
      phase: 2.1,
      colors: ["rgba(54, 9, 3, 0.28)", "rgba(151, 39, 7, 0.58)", "rgba(197, 81, 11, 0.64)"],
    },
    {
      base: 0.69,
      thickness: 0.2,
      amplitude: 0.058,
      frequency: 0.92,
      speed: 0.00017,
      phase: 4.4,
      colors: ["rgba(48, 7, 2, 0.3)", "rgba(119, 25, 5, 0.52)", "rgba(168, 49, 8, 0.58)"],
    },
    {
      base: 0.94,
      thickness: 0.24,
      amplitude: 0.045,
      frequency: 0.74,
      speed: -0.00013,
      phase: 5.7,
      colors: ["rgba(38, 6, 2, 0.32)", "rgba(91, 18, 4, 0.48)", "rgba(137, 35, 6, 0.54)"],
    },
  ];
  let width = 0;
  let height = 0;
  let animationFrame = 0;
  let canvasVisible = true;

  function resizeCanvas() {
    const ratio = Math.min(window.devicePixelRatio || 1, 1.5);
    width = canvas.clientWidth;
    height = canvas.clientHeight;
    canvas.width = Math.round(width * ratio);
    canvas.height = Math.round(height * ratio);
    context.setTransform(ratio, 0, 0, ratio, 0, 0);
  }

  /**
   * Возвращает вертикальную координату поверхности волны.
   *
   * Две синусоиды с разной частотой создают самостоятельное медленное течение.
   * Координата зависит только от времени: курсор не вмешивается в фон и не
   * перетягивает внимание с содержимого страницы на декоративную анимацию.
   */
  function getWaveY(wave, x, time, edge) {
    const progress = x / Math.max(width, 1);
    const flow = Math.sin(progress * Math.PI * 2 * wave.frequency + time * wave.speed + wave.phase);
    const detail = Math.sin(progress * Math.PI * 5.2 - time * wave.speed * 0.72 + wave.phase * 1.7);
    const baseline = wave.base * height;
    const texture =
      flow * wave.amplitude * height + detail * wave.amplitude * height * 0.24;
    const breathing = 1 + Math.sin(progress * Math.PI * 2.4 + wave.phase) * 0.1;
    const halfThickness = wave.thickness * height * breathing * 0.5;
    return baseline + texture + halfThickness * edge;
  }

  function traceWaveEdge(wave, time, edge, reverse = false) {
    const step = Math.max(8, width / 140);
    const points = [];
    for (let x = -step; x <= width + step; x += step) {
      points.push([x, getWaveY(wave, x, time, edge)]);
    }
    if (reverse) points.reverse();
    points.forEach(([x, y], index) => {
      if (index === 0 && !reverse) context.moveTo(x, y);
      else context.lineTo(x, y);
    });
  }

  /** Рисует цельную ленту и внутренний блик, подчёркивающий вязкость материала. */
  function drawWave(wave, time, index) {
    const fill = context.createLinearGradient(0, 0, width, 0);
    fill.addColorStop(0, wave.colors[1]);
    fill.addColorStop(0.48, wave.colors[2]);
    fill.addColorStop(1, wave.colors[2]);
    context.save();
    context.shadowColor = index === 1 ? "rgba(151, 44, 8, 0.3)" : "rgba(108, 24, 5, 0.2)";
    context.shadowBlur = index === 1 ? 34 : 24;
    context.fillStyle = fill;
    context.beginPath();
    traceWaveEdge(wave, time, -1);
    traceWaveEdge(wave, time, 1, true);
    context.closePath();
    context.fill();
    context.restore();

    const highlight = context.createLinearGradient(0, 0, width, 0);
    highlight.addColorStop(0, "rgba(176, 49, 9, 0.1)");
    highlight.addColorStop(0.5, "rgba(205, 69, 11, 0.16)");
    highlight.addColorStop(1, "rgba(230, 98, 14, 0.28)");
    context.strokeStyle = highlight;
    context.lineWidth = Math.max(1, height * 0.003);
    context.beginPath();
    traceWaveEdge(wave, time, -0.58);
    context.stroke();
  }

  function renderCanvas(time) {
    if (!canvasVisible) return;
    const animationTime = reducedMotion ? 0 : time;
    context.clearRect(0, 0, width, height);
    context.save();
    context.globalCompositeOperation = "screen";
    waves.forEach((wave, index) => drawWave(wave, animationTime, index));
    context.restore();
    animationFrame = window.requestAnimationFrame(renderCanvas);
  }

  /** Общий фон нужен на каждом слайде, но в скрытой вкладке цикл можно безопасно остановить. */
  document.addEventListener("visibilitychange", () => {
    canvasVisible = !document.hidden;
    if (canvasVisible && !animationFrame) {
      animationFrame = window.requestAnimationFrame(renderCanvas);
    } else if (!canvasVisible && animationFrame) {
      window.cancelAnimationFrame(animationFrame);
      animationFrame = 0;
    }
  });
  window.addEventListener("resize", resizeCanvas);
  resizeCanvas();
  animationFrame = window.requestAnimationFrame(renderCanvas);
})();
