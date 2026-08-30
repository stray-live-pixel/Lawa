(() => {
  "use strict";
  const slides = [...document.querySelectorAll("[data-slide]")];
  const arrows = [...document.querySelectorAll("[data-direction]")];
  const currentLabel = document.querySelector(".slide-progress__current");
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
  selectSlide(0);
  const copyButton = document.querySelector("[data-copy]");
  copyButton?.addEventListener("click", async () => {
    const command = copyButton.previousElementSibling.textContent.trim();
    try {
      await navigator.clipboard.writeText(command);
      copyButton.textContent = "Скопировано";
      window.setTimeout(() => (copyButton.textContent = "Копировать"), 1800);
    } catch {
      copyButton.textContent = "Выделите вручную";
    }
  });

  const parallaxLayers = [...document.querySelectorAll("[data-parallax]")];
  let parallaxFrame = 0;
  /** Смещает только декоративные изображения; контент всегда прокручивается браузером. */
  function renderParallax() {
    parallaxFrame = 0;
    const viewportCenter = window.innerHeight / 2;
    parallaxLayers.forEach((layer) => {
      const stage = layer.parentElement.getBoundingClientRect();
      const distance = viewportCenter - (stage.top + stage.height / 2);
      const offset = reducedMotion ? 0 : distance * Number(layer.dataset.parallax);
      layer.style.transform = `translate(-50%, calc(-50% + ${offset}px))`;
    });
  }

  function scheduleParallax() {
    if (!parallaxFrame) parallaxFrame = window.requestAnimationFrame(renderParallax);
  }

  window.addEventListener("scroll", scheduleParallax, { passive: true });
  window.addEventListener("resize", scheduleParallax);
  renderParallax();
  const canvas = document.querySelector(".lava-canvas");
  if (!canvas) return;
  const context = canvas.getContext("2d", { alpha: true });
  if (!context) return;
  const hero = document.querySelector(".slide--hero");
  const pointer = {
    x: 0.78,
    y: 0.5,
    targetX: 0.78,
    targetY: 0.5,
    influence: 0,
    active: false,
  };
  const waves = [
    {
      base: 0.14,
      thickness: 0.12,
      amplitude: 0.035,
      frequency: 1.35,
      speed: 0.00026,
      phase: 0.4,
      colors: ["rgba(255, 61, 0, 0)", "rgba(255, 78, 0, 0.68)", "rgba(255, 166, 0, 0.86)"],
    },
    {
      base: 0.4,
      thickness: 0.16,
      amplitude: 0.048,
      frequency: 1.12,
      speed: -0.0002,
      phase: 2.1,
      colors: ["rgba(255, 61, 0, 0)", "rgba(255, 90, 0, 0.74)", "rgba(255, 184, 0, 0.94)"],
    },
    {
      base: 0.69,
      thickness: 0.2,
      amplitude: 0.058,
      frequency: 0.92,
      speed: 0.00017,
      phase: 4.4,
      colors: ["rgba(143, 28, 0, 0)", "rgba(255, 61, 0, 0.66)", "rgba(255, 119, 0, 0.88)"],
    },
    {
      base: 0.94,
      thickness: 0.24,
      amplitude: 0.045,
      frequency: 0.74,
      speed: -0.00013,
      phase: 5.7,
      colors: ["rgba(111, 17, 0, 0)", "rgba(211, 47, 0, 0.52)", "rgba(255, 90, 0, 0.76)"],
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
   * Две синусоиды создают живое течение без повторяющегося «эквалайзера».
   * Курсор не тянет отдельную точку: он локально отталкивает весь слой и запускает
   * затухающую рябь. Поэтому эффект остаётся непрерывной жидкостью.
   */
  function getWaveY(wave, x, time, edge) {
    const startX = width < 640 ? width * 0.38 : width * 0.42;
    const span = Math.max(width - startX, 1);
    const progress = (x - startX) / span;
    const flow = Math.sin(progress * Math.PI * 2 * wave.frequency + time * wave.speed + wave.phase);
    const detail = Math.sin(progress * Math.PI * 5.2 - time * wave.speed * 0.72 + wave.phase * 1.7);
    const naturalY =
      wave.base * height +
      flow * wave.amplitude * height +
      detail * wave.amplitude * height * 0.24;
    const pointerX = pointer.x * width;
    const pointerY = pointer.y * height;
    const radius = Math.max(Math.min(width, height) * 0.28, 180);
    const horizontalDistance = (x - pointerX) / radius;
    const proximity = Math.exp(-horizontalDistance * horizontalDistance * 2.7) * pointer.influence;
    const repulsion = (naturalY - pointerY) * proximity * 0.48;
    const ripple =
      Math.sin(Math.abs(horizontalDistance) * 8.5 - time * 0.004) *
      height *
      0.013 *
      proximity;
    const breathing = 1 + Math.sin(progress * Math.PI * 2.4 + wave.phase) * 0.12;
    const halfThickness = wave.thickness * height * breathing * (0.5 + proximity * 0.12);
    return naturalY + repulsion + ripple + halfThickness * edge;
  }

  function traceWaveEdge(wave, time, edge, reverse = false) {
    const startX = width < 640 ? width * 0.34 : width * 0.38;
    const step = Math.max(8, width / 140);
    const points = [];
    for (let x = startX; x <= width + step; x += step) {
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
    const startX = width < 640 ? width * 0.34 : width * 0.38;
    const fill = context.createLinearGradient(startX, 0, width, 0);
    fill.addColorStop(0, wave.colors[0]);
    fill.addColorStop(0.38, wave.colors[1]);
    fill.addColorStop(1, wave.colors[2]);
    context.save();
    context.shadowColor = index === 1 ? "rgba(255, 111, 0, 0.42)" : "rgba(255, 61, 0, 0.24)";
    context.shadowBlur = index === 1 ? 38 : 26;
    context.fillStyle = fill;
    context.beginPath();
    traceWaveEdge(wave, time, -1);
    traceWaveEdge(wave, time, 1, true);
    context.closePath();
    context.fill();
    context.restore();

    const highlight = context.createLinearGradient(startX, 0, width, 0);
    highlight.addColorStop(0, "rgba(255, 207, 128, 0)");
    highlight.addColorStop(0.5, "rgba(255, 214, 136, 0.2)");
    highlight.addColorStop(1, "rgba(255, 239, 185, 0.62)");
    context.strokeStyle = highlight;
    context.lineWidth = Math.max(1, height * 0.003);
    context.beginPath();
    traceWaveEdge(wave, time, -0.58);
    context.stroke();
  }

  function renderCanvas(time) {
    if (!canvasVisible) return;
    const animationTime = reducedMotion ? 0 : time;
    pointer.x += (pointer.targetX - pointer.x) * 0.085;
    pointer.y += (pointer.targetY - pointer.y) * 0.085;
    pointer.influence += ((pointer.active && !reducedMotion ? 1 : 0) - pointer.influence) * 0.07;
    context.clearRect(0, 0, width, height);
    context.save();
    context.globalCompositeOperation = "screen";
    waves.forEach((wave, index) => drawWave(wave, animationTime, index));
    context.restore();
    animationFrame = window.requestAnimationFrame(renderCanvas);
  }

  canvas.addEventListener("pointermove", (event) => {
    const bounds = canvas.getBoundingClientRect();
    pointer.targetX = (event.clientX - bounds.left) / bounds.width;
    pointer.targetY = (event.clientY - bounds.top) / bounds.height;
    pointer.active = true;
  });

  canvas.addEventListener("pointerleave", () => (pointer.active = false));
  // Вне первого экрана анимация полностью останавливается и не расходует ресурсы.
  new IntersectionObserver(([entry]) => {
    canvasVisible = entry.isIntersecting;
    if (canvasVisible && !animationFrame) {
      animationFrame = window.requestAnimationFrame(renderCanvas);
    } else if (!canvasVisible && animationFrame) {
      window.cancelAnimationFrame(animationFrame);
      animationFrame = 0;
    }
  }).observe(hero);
  window.addEventListener("resize", resizeCanvas);
  resizeCanvas();
  animationFrame = window.requestAnimationFrame(renderCanvas);
})();
