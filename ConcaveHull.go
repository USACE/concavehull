package ConcaveHull

/**
	Golang implementation of https://github.com/skipperkongen/jts-algorithm-pack/blob/master/src/org/geodelivery/jap/concavehull/SnapHull.java
	which is a Java port of st_concavehull from Postgis 2.0
 */

import (
	"sort"
	"github.com/furstenheim/go-convex-hull-2d"
	"sync"
	"github.com/furstenheim/SimpleRTree"
	"math"
	"github.com/paulmach/go.geo"
	"github.com/paulmach/go.geo/reducers"
)

const DEFAULT_SEGLENGTH = 0.001
type concaver struct {
	rtree * SimpleRTree.SimpleRTree
	seglength float64
	closestPointsMem []closestPoint
	searchItemsMem []searchItem
}
type Options struct {
	Seglength float64
	BaseArrayPool, SorterBufferPool *sync.Pool // This will be passed down to RTree useful for high concurrency server
}
func Compute (points FlatPoints) (concaveHull FlatPoints) {
	return ComputeWithOptions(points, nil)
}
func ComputeWithOptions (points FlatPoints, o *Options) (concaveHull FlatPoints) {
	sort.Sort(lexSorter(points))
	return ComputeFromSortedWithOptions(points, o)
}
func ComputeFromSorted (points FlatPoints) (concaveHull FlatPoints) {
	return ComputeFromSortedWithOptions(points, nil)
}

// Compute concave hull from sorted points. Points are expected to be sorted lexicographically by (x,y)
func ComputeFromSortedWithOptions (points FlatPoints, o *Options) (concaveHull FlatPoints) {
	// Create a copy so that convex hull and index can modify the array in different ways
	pointsCopy := make(FlatPoints, 0, len(points))
	pointsCopy = append(pointsCopy, points...)
	var rtreeOptions SimpleRTree.Options
	if o != nil {
		rtreeOptions.BaseArrayPool = o.BaseArrayPool
		rtreeOptions.SorterBufferPool = o.SorterBufferPool
	}
	rtreeOptions.UnsafeConcurrencyMode = true // we only access from one goroutine at a time
	rtree := SimpleRTree.NewWithOptions(rtreeOptions)
	var wg sync.WaitGroup
	wg.Add(2)
	// Convex hull
	go func () {
		points = go_convex_hull_2d.NewFromSortedArray(points).(FlatPoints)
		wg.Done()
	}()

	func () {
		rtree.LoadSortedArray(SimpleRTree.FlatPoints(pointsCopy))
		wg.Done()
	}()
	wg.Wait()
	var c concaver
	c.seglength = DEFAULT_SEGLENGTH
	if o != nil && o.Seglength != 0 {
		c.seglength = o.Seglength
	}
	c.rtree = rtree
	c.closestPointsMem = make([]closestPoint, 0 , 2)
	c.searchItemsMem = make([]searchItem, 0 , 2)
	result := c.computeFromSorted(points)
	rtree.Destroy() // free resources
	return result
}

func (c * concaver) computeFromSorted (convexHull FlatPoints) (concaveHull FlatPoints) {
	// degerated case
	if (convexHull.Len() < 3) {
		return convexHull
	}
	concaveHull = make([]float64, 0, 2 * convexHull.Len())
	x0, y0 := convexHull.Take(0)
	concaveHull = append(concaveHull, x0, y0)
	for i := 0; i<convexHull.Len(); i++ {
		x1, y1 := convexHull.Take(i)
		var x2, y2 float64
		if i == convexHull.Len() -1 {
			x2, y2 = convexHull.Take(0)
		} else {
			x2, y2 = convexHull.Take(i + 1)
		}
		sideSplit := c.segmentize(x1, y1, x2, y2)
		for _, p := range(sideSplit) {
			concaveHull = append(concaveHull, p.x, p.y)
		}
	}
	path := reducers.DouglasPeucker(geo.NewPathFromFlatXYData(concaveHull), c.seglength)
	// reused allocated array
	concaveHull = concaveHull[0:0]
	reducedPoints := path.Points()

	for _, p := range(reducedPoints) {
		concaveHull = append(concaveHull, p.Lng(), p.Lat())
	}
	return concaveHull
}

// Split side in small edges, for each edge find closest point. Remove duplicates
func (c * concaver) segmentize (x1, y1, x2, y2 float64) (points []closestPoint) {
	dist := math.Sqrt((x1 - x2) * (x1 - x2) + (y1 - y2) * (y1 - y2))
	nSegments := math.Ceil(dist / c.seglength)
	factor := 1 / nSegments
	vX := factor * (x2 - x1)
	vY := factor * (y2 - y1)

	closestPoints := c.closestPointsMem[0: 0]
	closestPoints = append(closestPoints, closestPoint{index: 0, x: x1, y: y1})
	closestPoints = append(closestPoints, closestPoint{index: int(nSegments), x: x2, y: y2})

	if (nSegments < 2) {
		return closestPoints[1:]
	}

	stack := c.searchItemsMem[0: 0]
	stack = append(stack, searchItem{left: 0, right: int(nSegments), lastLeftIndex: 0, lastRightIndex: 1})
	for len(stack) > 0 {
		var item searchItem
		item, stack = stack[len(stack)-1], stack[:len(stack)-1]
		if item.right - item.left <= 1 {
			continue
		}
		index := (item.left + item.right) / 2
		fIndex := float64(index)
		currentX := x1 + vX * fIndex
		currentY := y1 + vY * fIndex
		d1 := vX * fIndex * vX * fIndex + vY * fIndex * vY * fIndex + 0.0001
		d2 := vX * (nSegments - fIndex) * vX * (nSegments - fIndex) + vY * (nSegments - fIndex) * vY * (nSegments - fIndex) + 0.0001
		x, y, _, _ := c.rtree.FindNearestPointWithin(currentX, currentY, math.Min(d1, d2))
		isNewLeft := x != closestPoints[item.lastLeftIndex].x || y != closestPoints[item.lastLeftIndex].y
		isNewRight := x != closestPoints[item.lastRightIndex].x || y != closestPoints[item.lastRightIndex].y

		// we don't know the point
		if isNewLeft && isNewRight {
			newResultIndex := len(closestPoints)
			closestPoints = append(closestPoints, closestPoint{index: index, x: x, y: y})
			stack = append(stack, searchItem{left: item.left, right: index, lastLeftIndex: item.lastLeftIndex, lastRightIndex: newResultIndex})
			// alloc
			stack = append(stack, searchItem{left: index, right: item.right, lastLeftIndex: newResultIndex, lastRightIndex: item.lastRightIndex})
		} else if (isNewLeft) {
			stack = append(stack, searchItem{left: item.left, right: index, lastLeftIndex: item.lastLeftIndex, lastRightIndex: item.lastRightIndex})
		} else if (isNewRight) {
			// don't add point to closest points, but we need to keep looking on the right side
			stack = append(stack, searchItem{left: index, right: item.right, lastLeftIndex: item.lastLeftIndex, lastRightIndex: item.lastRightIndex})
		}
	}
	sort.Sort(closestPointSorter(closestPoints))
	c.searchItemsMem = stack
	c.closestPointsMem = closestPoints
	return closestPoints[1:]
}

type closestPoint struct {
	index int
	x, y float64
}

type searchItem struct {
	left, right, lastLeftIndex, lastRightIndex int

}





type FlatPoints []float64

func (fp FlatPoints) Len () int {
	return len(fp) / 2
}

func (fp FlatPoints) Slice (i, j int) (go_convex_hull_2d.Interface) {
	return fp[2 * i: 2 * j]
}

func (fp FlatPoints) Swap (i, j int) {
	fp[2 * i], fp[2 * i + 1], fp[2 * j], fp[2 * j + 1] = fp[2 * j], fp[2 * j + 1], fp[2 * i], fp[2 * i + 1]
}

func (fp FlatPoints) Take(i int) (x1, y1 float64) {
	return fp[2 * i], fp[2 * i +1]
}
