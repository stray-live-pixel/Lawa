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
  const hero = document.querySelector(".slide--hero");
  const pointer = { x: 0.72, y: 0.5, active: false };
  let blobs = [];
  let width = 0;
  let height = 0;
  let animationFrame = 0;
  let previousTime = 0;
  let canvasVisible = true;
  /** Создаёт устойчивый набор потоков: позиции нормализованы и переживают resize. */
  function createBlobs() {
    const count = width < 640 ? 6 : 10;
    blobs = Array.from({ length: count }, (_, index) => ({
      x: 0.52 + Math.random() * 0.56,
      y: 0.08 + Math.random() * 0.84,
      radius: 0.075 + Math.random() * 0.1,
      vx: (Math.random() - 0.5) * 0.022,
      vy: (Math.random() - 0.5) * 0.018,
      phase: index * 0.74 + Math.random(),
    }));
  }

  function resizeCanvas() {
    const ratio = Math.min(window.devicePixelRatio || 1, 1.5);
    width = canvas.clientWidth;
    height = canvas.clientHeight;
    canvas.width = Math.round(width * ratio);
    canvas.height = Math.round(height * ratio);
    context.setTransform(ratio, 0, 0, ratio, 0, 0);
    if (!blobs.length) createBlobs();
  }

  function updateBlob(blob, delta, time) {
    blob.x += blob.vx * delta;
    blob.y += blob.vy * delta + Math.sin(time * 0.00035 + blob.phase) * 0.00012 * delta;
    if (pointer.active) {
      const dx = pointer.x - blob.x;
      const dy = pointer.y - blob.y;
      const distance = Math.max(Math.hypot(dx, dy), 0.08);
      if (distance < 0.3) {
        blob.x -= (dx / distance) * 0.00045 * delta;
        blob.y -= (dy / distance) * 0.00045 * delta;
      }
    }
    if (blob.x < 0.45 || blob.x > 1.12) blob.vx *= -1;
    if (blob.y < -0.08 || blob.y > 1.08) blob.vy *= -1;
    blob.x = Math.max(0.43, Math.min(1.14, blob.x));
    blob.y = Math.max(-0.1, Math.min(1.1, blob.y));
  }

  /** Рисует вязкие перемычки между близкими каплями, превращая круги в один поток. */
  function drawConnections(points) {
    points.forEach((first, firstIndex) => {
      points.slice(firstIndex + 1).forEach((second) => {
        const distance = Math.hypot(first.x - second.x, first.y - second.y);
        const limit = (first.radius + second.radius) * 1.32;
        if (distance >= limit) return;
        const intensity = 1 - distance / limit;
        const gradient = context.createLinearGradient(first.x, first.y, second.x, second.y);
        gradient.addColorStop(0, `rgba(255, 74, 0, ${0.24 * intensity})`);
        gradient.addColorStop(0.5, `rgba(255, 122, 0, ${0.52 * intensity})`);
        gradient.addColorStop(1, `rgba(255, 184, 0, ${0.22 * intensity})`);
        context.strokeStyle = gradient;
        context.lineWidth = Math.min(first.radius, second.radius) * intensity * 0.95;
        context.lineCap = "round";
        context.beginPath();
        context.moveTo(first.x, first.y);
        context.lineTo(second.x, second.y);
        context.stroke();
      });
    });
  }

  function drawBlob(blob) {
    const x = blob.x * width;
    const y = blob.y * height;
    const radius = blob.radius * Math.min(width, height);
    const glow = context.createRadialGradient(
      x - radius * 0.22,
      y - radius * 0.28,
      radius * 0.03,
      x,
      y,
      radius,
    );
    glow.addColorStop(0, "rgba(255, 231, 137, 0.95)");
    glow.addColorStop(0.16, "rgba(255, 184, 0, 0.92)");
    glow.addColorStop(0.48, "rgba(255, 90, 0, 0.72)");
    glow.addColorStop(0.76, "rgba(130, 25, 0, 0.26)");
    glow.addColorStop(1, "rgba(45, 8, 0, 0)");
    context.fillStyle = glow;
    context.beginPath();
    context.arc(x, y, radius, 0, Math.PI * 2);
    context.fill();
  }

  function renderCanvas(time) {
    if (!canvasVisible) return;
    const delta = Math.min((time - previousTime) / 16.67 || 1, 2.2);
    previousTime = time;
    context.clearRect(0, 0, width, height);
    context.save();
    context.globalCompositeOperation = "screen";
    blobs.forEach((blob) => updateBlob(blob, reducedMotion ? 0 : delta, time));
    const points = blobs.map((blob) => ({
      x: blob.x * width,
      y: blob.y * height,
      radius: blob.radius * Math.min(width, height),
    }));
    drawConnections(points);
    blobs.forEach(drawBlob);
    context.restore();
    animationFrame = window.requestAnimationFrame(renderCanvas);
  }

  canvas.addEventListener("pointermove", (event) => {
    const bounds = canvas.getBoundingClientRect();
    pointer.x = (event.clientX - bounds.left) / bounds.width;
    pointer.y = (event.clientY - bounds.top) / bounds.height;
    pointer.active = true;
  });

  canvas.addEventListener("pointerleave", () => (pointer.active = false));
  // Вне первого экрана анимация полностью останавливается и не расходует ресурсы.
  new IntersectionObserver(([entry]) => {
    canvasVisible = entry.isIntersecting;
    if (canvasVisible && !animationFrame) {
      previousTime = 0;
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
