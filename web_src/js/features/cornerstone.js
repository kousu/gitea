// CornerstoneJS is a renderer and UI toolkit meant for
//  medical image formats: .nii, .dcm, and some others.
//
// To use: create an element like
//
// * <div class='.cornerstone-viewport' data-src='nifti:/path/to/file.nii.gz' style='width: 100%; height 500px'></div>
//
// and file.nii.gz will load when the page loads.
//
// Swap out 'nifti' for 'wadouri:' if using a DICOM file.
//
//
// **NOTE**: this is written using the _legacy_ cornerstone library,
//           cornerstone-core, and is *not* compatible with the rewrite
//           @cornerstonejs/core because @cornerstonejs/nifti-image-loader
//           isn't: https://github.com/cornerstonejs/cornerstone-nifti-image-loader/issues/48#issuecomment-1271794397
//           and rendering NIfTIs is the main reason this is here at all.
//

export async function initCornerstone() {
  if (!document.querySelector('.cornerstone-viewport')) return;

  const cornerstone = await import(/* webpackChunkName: "cornerstone-core" */ 'cornerstone-core');
  const dicomParser = await import(/* webpackChunkName: "dicom-parser" */ 'dicom-parser');
  const cornerstoneWADOImageLoader = await import(/* webpackChunkName: "cornerstone-wado-image-loader" */ 'cornerstone-wado-image-loader');
  const cornerstoneNIFTIImageLoader = await import(/* webpackChunkName: "@cornerstonejs/nifti-image-loader" */ '@cornerstonejs/nifti-image-loader');

  // why are these necessary? too much abstraction, that's why.
  cornerstoneWADOImageLoader.external.dicomParser = dicomParser;
  cornerstoneWADOImageLoader.external.cornerstone = cornerstone;
  cornerstoneNIFTIImageLoader.external.cornerstone = cornerstone;

  for (const canvas of document.querySelectorAll('.cornerstone-viewport')) {
    if (canvas.dataset.src === undefined) {
      continue;
    }
    cornerstone.enable(canvas);

    console.info('Loading', canvas.dataset.src, 'into CornerstoneJS');
    cornerstone.loadAndCacheImage(canvas.dataset.src)
      .then((image) => {
        console.debug(image);

        const viewport = cornerstone.getDefaultViewportForImage(canvas, image);
        cornerstone.displayImage(canvas, image, viewport);
      })
      .catch((err) => {
        console.error(err);
      });
  }
}
